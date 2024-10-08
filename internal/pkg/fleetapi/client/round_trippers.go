// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package client

import (
	"errors"
	"net/http"

	"github.com/elastic/elastic-agent/internal/pkg/remote"
)

// ErrInvalidAPIKey is returned when authentication fail to fleet.
var ErrInvalidAPIKey = errors.New("invalid api key to authenticate with fleet")

// FleetUserAgentRoundTripper adds the Fleet user agent.
type FleetUserAgentRoundTripper struct {
	rt http.RoundTripper
}

// RoundTrip adds the Fleet user agent string to every request.
func (r *FleetUserAgentRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return r.rt.RoundTrip(req)
}

// NewFleetUserAgentRoundTripper returns a  FleetUserAgentRoundTripper that actually wrap the
// existing UserAgentRoundTripper with a specific string.
func NewFleetUserAgentRoundTripper(wrapped http.RoundTripper, version string) http.RoundTripper {
	const name = "Elastic Agent"
	return &FleetUserAgentRoundTripper{
		rt: remote.NewUserAgentRoundTripper(wrapped, name+" v"+version),
	}
}

// FleetAuthRoundTripper allow all calls to be authenticated using the api key.
// The token is added as a header key.
type FleetAuthRoundTripper struct {
	rt     http.RoundTripper
	apiKey string
}

// RoundTrip makes all the calls to the service authenticated.
func (r *FleetAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	const key = "Authorization"
	const prefix = "ApiKey "

	req.Header.Set(key, prefix+r.apiKey)
	resp, err := r.rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		defer resp.Body.Close()
		return nil, ErrInvalidAPIKey
	}

	return resp, err
}

// NewFleetAuthRoundTripper wrap an existing http.RoundTripper and adds the API in the header.
func NewFleetAuthRoundTripper(
	wrapped http.RoundTripper,
	apiKey string,
) (http.RoundTripper, error) {
	if len(apiKey) == 0 {
		return nil, errors.New("empty api key received")
	}
	return &FleetAuthRoundTripper{rt: wrapped, apiKey: apiKey}, nil
}

// ElasticApiVersionRoundTripper adds an Elastic-Api-Version header on every request.
type ElasticApiVersionRoundTripper struct {
	rt                http.RoundTripper
	elasticApiVersion string
}

const elasticApiVersionHeaderKey = "Elastic-Api-Version"

// RoundTrip adds an Elastic-Api-Version header on every request.
func (r *ElasticApiVersionRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set(elasticApiVersionHeaderKey, r.elasticApiVersion)

	return r.rt.RoundTrip(req)
}

func NewElasticApiVersionRoundTripper(inner http.RoundTripper, elasticApiVersion string) http.RoundTripper {
	return &ElasticApiVersionRoundTripper{elasticApiVersion: elasticApiVersion, rt: inner}
}
