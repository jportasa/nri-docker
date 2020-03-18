package aws

// Copyright 2017-2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"time"
)

const (
	containerMetadataEnvVar = "ECS_CONTAINER_METADATA_URI"
	maxRetries              = 4
	durationBetweenRetries  = time.Second
)

// metadataResponse gets the response from the given endpoint using the given HTTP client.
func metadataResponse(client *http.Client, endpoint string) ([]byte, error) {
	var resp []byte
	var err error
	for i := 0; i < maxRetries; i++ {
		resp, err = sendMetadataRequest(client, endpoint)
		if err == nil {
			return resp, nil
		}
		fmt.Fprintf(os.Stderr, "Attempt [%d/%d]: unable to get metadata response from '%s': %v",
			i, maxRetries, endpoint, err)
		time.Sleep(durationBetweenRetries)
	}

	return nil, err
}

func sendMetadataRequest(client *http.Client, endpoint string) ([]byte, error) {
	resp, err := client.Get(endpoint)
	if err != nil {
		return nil, fmt.Errorf("unable to get response from %s: %v", endpoint, err)
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("incorrect status code querying %s: %d", endpoint, resp.StatusCode)
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("unable to read response body from %s: %v", endpoint, err)
	}

	return body, nil
}

// TaskMetadataEndpoint returns the V3 endpoint to fetch task metadata.
func TaskStatsEndpoint() (string, bool) {
	baseEndpoint, found := metadataV3Endpoint()
	if !found {
		return "", found
	}
	return baseEndpoint + "/task/stats", found
}

// metadataV3Endpoint returns the v3 metadata endpoint configured via the ECS_CONTAINER_METADATA_URI environment
// variable.
func metadataV3Endpoint() (string, bool) {
	return os.LookupEnv(containerMetadataEnvVar)
}
