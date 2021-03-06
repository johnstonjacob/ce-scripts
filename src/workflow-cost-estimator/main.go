package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

// Response is of type APIGatewayProxyResponse since we're leveraging the
// AWS Lambda Proxy Request functionality (default behavior)
// https://serverless.com/framework/docs/providers/aws/events/apigateway/#lambda-proxy-integration
type Response events.APIGatewayProxyResponse

// Handler is our lambda handler invoked by the `lambda.Start` function call
func Handler(ctx context.Context, request events.APIGatewayProxyRequest) (Response, error) {
	var responseBody responseBody

	params, urls, err := paramSetup(request)
	if err != nil {
		return *generateResponse(responseBody, err), nil
	}

	err = getWorkflowStatus(urls, params)
	if err != nil {
		return *generateResponse(responseBody, err), nil
	}

	jobs, err := getWorkflowJobs(urls, params)
	if err != nil {
		return *generateResponse(responseBody, err), nil
	}

	responseBody, err = tallyJobCost(jobs, urls, params)

	if err != nil {
		return *generateResponse(responseBody, err), nil
	}

	return *generateResponse(responseBody, nil), nil
}

// TODO better variable names
func generateResponse(responseBody responseBody, err error) *Response {
	var buf bytes.Buffer
	var bodyBytes []byte
	var statusCode int

	if err, ok := err.(responseErr); ok {
		statusCode = err.statusCode
		errBody, err := json.Marshal(map[string]interface{}{
			"error": err.Error(),
		})

		if err != nil {
			return generateResponse(responseBody, responseErr{err: err.Error(), statusCode: 500})
		}
		bodyBytes = errBody
	} else {
		responseBytes, err := json.Marshal(responseBody)
		bodyBytes = responseBytes
		statusCode = 200

		if err != nil {
			return generateResponse(responseBody, responseErr{err: err.Error(), statusCode: 500})
		}
	}

	json.HTMLEscape(&buf, bodyBytes)

	return &Response{
		StatusCode:      statusCode,
		IsBase64Encoded: false,
		Body:            buf.String(),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}
}

type jobTuple struct {
	cost float64
	time time.Duration
	name string
}

// TODO seperate logic and make this function (more) testable
func tallyJobCost(jobs workflowJobsResponse, urls circleURLs, params queryParameters) (responseBody, error) {
	var totalCredits float64
	var totalTime time.Duration
	var jobsRes []job
	var wg sync.WaitGroup
	var errors []error

	wg.Add(len(jobs.Jobs))

	// TODO: determine ideal chan buffer
	c := make(chan jobTuple, 4)
	ec := make(chan error, 4)

	for _, job := range jobs.Jobs {
		go func(job Jobs) {
			if job.Status == "blocked" {
				c <- jobTuple{0, 0, fmt.Sprintf("Blocked - %s", job.Name)}
				return
			}

			jobURL := fmt.Sprintf("%sproject/%s/%d", urls.v1URL, job.ProjectSlug, job.JobNumber)
			cost, time, err := getJobDetails(jobURL, params)

			if err != nil {
				ec <- err
				return
			}
			c <- jobTuple{cost, time, job.Name}
		}(job)
	}

	go func(totalCredits *float64, totalTime *time.Duration, jobsRes *[]job) {
		for details := range c {
			*totalCredits += details.cost
			*totalTime += details.time
			totalPrice := math.Ceil(creditCost(details.cost)*100) / 100
			*jobsRes = append(*jobsRes, job{details.name, totalPrice, details.cost, details.time.String()})
			wg.Done()
		}

	}(&totalCredits, &totalTime, &jobsRes)

	go func(errors *[]error) {
		for err := range ec {
			*errors = append(*errors, err)
			wg.Done()
		}
	}(&errors)

	wg.Wait()

	if len(errors) > 0 {
		return responseBody{}, responseErr{fmt.Sprintf("Error retrieving job details: %+v", errors), 400}
	}

	totalCredits = math.Ceil(totalCredits)
	totalPrice := math.Ceil(creditCost(totalCredits)*100) / 100

	return *newResponseBody(totalCredits, totalPrice, totalTime, jobsRes), nil
}

func getJobDetails(url string, params queryParameters) (float64, time.Duration, error) {
	var response jobDetailResponse
	var buildTime time.Duration

	resp, err := makeBasicAuthRequest(url, params["circleToken"])
	defer resp.Body.Close()

	if err != nil {
		return 0, 0, err
	}

	err = unmarshalAPIResp(resp, &response)
	if err != nil {
		return 0, 0, err
	}

	name := response.Workflows.JobName
	resourceClass := response.Picard.ResourceClass.Class
	executor := response.Picard.Executor
	creditPerMin, err := lookupCreditPerMin(executor, resourceClass, name)

	if err != nil {
		return 0, 0, err
	}

	for _, step := range response.Steps {
		for _, action := range step.Actions {
			if action.Background {
				continue
			}

			buildTime += time.Duration(action.RunTimeMillis) * time.Millisecond
		}
	}

	buildTime = buildTime.Round(time.Second)

	credits := buildTime.Minutes() * creditPerMin

	return credits, buildTime, nil
}

func getWorkflowJobs(urls circleURLs, params queryParameters) (workflowJobsResponse, error) {
	var response workflowJobsResponse
	workflowJobsURL := fmt.Sprintf("%sworkflow/%s/jobs", urls.v2URL, params["workflowID"])

	resp, err := makeBasicAuthRequest(workflowJobsURL, params["circleToken"])
	defer resp.Body.Close()

	if err != nil {
		return response, err
	}

	err = unmarshalAPIResp(resp, &response)

	if err != nil {
		return response, err
	}

	return response, err
}

func getWorkflowStatus(urls circleURLs, params queryParameters) error {
	var response workflowResponse
	workflowURL := fmt.Sprintf("%sworkflow/%s", urls.v2URL, params["workflowID"])

	resp, err := makeBasicAuthRequest(workflowURL, params["circleToken"])
	defer resp.Body.Close()

	if err != nil {
		return err
	}

	err = unmarshalAPIResp(resp, &response)

	if err != nil {
		return err
	}

	if response.Status != "success" && response.Status != "failed" {
		return responseErr{fmt.Sprintf("Workflow status is %s. Status must be 'success' or 'failed' to estimate cost", response.Status), 201}
	}

	return nil
}

func unmarshalAPIResp(resp *http.Response, f interface{}) error {
	var bodyString string

	if resp.StatusCode == http.StatusOK {
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return responseErr{fmt.Sprintf("Error reading API response. Error: %s", err), 500}
		}
		bodyString = string(bodyBytes)
	} else {
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return responseErr{fmt.Sprintf("Error reading API response. Error: %s", err), 500}
		}

		bodyString = string(bodyBytes)
		return responseErr{fmt.Sprintf("Bad status code from CCI API response. Status code: %d, CCI Error: %s", resp.StatusCode, bodyString), 500}
	}

	if err := json.Unmarshal([]byte(bodyString), &f); err != nil {
		return responseErr{fmt.Sprintf("Error unmarshalling JSON resposnse. Error: %s", err), 500}
	}

	return nil
}

func basicAuth(username, password string) string {
	auth := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}

func makeBasicAuthRequest(url string, token string) (*http.Response, error) {
	client := &http.Client{
		Timeout: time.Second * 10,
	}

	req, err := http.NewRequest("GET", url, nil)

	if err != nil {
		return nil, responseErr{fmt.Sprintf("Error creating HTTP client with provided URL. URL: %s. Error: %s", url, err), 500}
	}

	req.Header.Add("Authorization", "Basic "+basicAuth(token, ""))
	req.Header.Add("Accept", "application/json")

	resp, err := client.Do(req)

	if err != nil {
		return nil, responseErr{fmt.Sprintf("Error getting requested URL. URL: %s. Error: %s", url, err), 500}
	}

	return resp, nil
}

func creditCost(credits float64) float64 {
	return credits * 0.0006
}

func lookupCreditPerMin(executor, resourceClass, jobName string) (float64, error) {
	var creditPerMin float64
	var ok bool

	// TODO add all resource classes
	var resourceClasses = map[string]map[string]float64{
		"docker": {
			"small":    5,
			"medium":   10,
			"medium+":  15,
			"large":    20,
			"xlarge":   40,
			"2xlarge":  80,
			"2xlarge+": 100,
			"3xlarge":  160,
			"4xlarge":  320,
		},
		"machine": {
			"small":          5,
			"medium":         10,
			"large":          20,
			"xlarge":         40,
			"2xlarge":        80,
			"3xlarge":        120,
			"gpu.small":      80,
			"gpu.medium":     160,
			"gpu.large":      320,
			"windows.medium": 40,
		},
		"macos": {
			"small":  25,
			"medium": 50,
			"large":  100,
		},
		"windows": {},
	}

	if _, ok = resourceClasses[executor]; !ok {
		return 0, responseErr{fmt.Sprintf("Missing Executor. Please contact jacobjohnston@circleci.com with this error message, your parameters, and executor type. Executor: %s", executor), 500}
	}
	if creditPerMin, ok = resourceClasses[executor][resourceClass]; !ok {
		return 0, responseErr{fmt.Sprintf("Missing resource class cost for %s:%s in job %s", executor, resourceClass, jobName), 500}
	}
	return creditPerMin, nil
}

func snakeCaseToCamelCase(input string) (output string) {
	isToUpper := false

	for _, v := range input {
		if isToUpper && v != '_' {
			output += strings.ToUpper(string(v))
			isToUpper = false
		} else {
			if v == '_' {
				isToUpper = true
			} else {
				output += string(v)
			}
		}
	}
	return

}

func paramSetup(request events.APIGatewayProxyRequest) (queryParameters, circleURLs, error) {
	var urls circleURLs
	var ok bool
	qs := request.QueryStringParameters
	params := make(queryParameters)

	requiredParams := []string{"circle_token"}

	for _, v := range requiredParams {
		if _, ok = qs[v]; !ok {
			return params, urls, responseErr{fmt.Sprintf("Please provide query parameters: %s", strings.Join(requiredParams, ", ")), 400}

		}
		p := snakeCaseToCamelCase(v)
		params[p] = qs[v]
	}

	params["workflowID"] = request.PathParameters["workflow_id"]

	// TODO: seperate url logic
	if qs["circle_url"] == "" {
		urls.circleURL = "https://circleci.com"
	} else {
		urls.circleURL = qs["circle_url"]
	}

	urls.v1URL = fmt.Sprintf("%s/api/v1.1/", urls.circleURL)
	urls.v2URL = fmt.Sprintf("%s/api/v2/", urls.circleURL)

	return params, urls, nil
}

func main() {
	lambda.Start(Handler)
}
