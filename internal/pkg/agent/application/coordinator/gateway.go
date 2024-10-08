// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package coordinator

import (
	"context"

	"github.com/elastic/elastic-agent/internal/pkg/fleetapi"
	"github.com/elastic/elastic-agent/internal/pkg/fleetapi/client"
)

// FleetGateway is a gateway between the Agent and the Fleet API, it's take cares of all the
// bidirectional communication requirements. The gateway aggregates events and will periodically
// call the API to send the events and will receive actions to be executed locally.
// The only supported action for now is a "ActionPolicyChange".
type FleetGateway interface {
	// Run runs the gateway.
	Run(ctx context.Context) error

	// Errors returns the channel to watch for reported errors.
	Errors() <-chan error

	// Actions returns the channel to watch for new actions from the fleet-server.
	Actions() <-chan []fleetapi.Action

	// SetClient sets the client for the gateway.
	SetClient(client.Sender)
}

// WarningError is emitted when we receive a warning in the Fleet response
type WarningError struct {
	msg string
}

func (w WarningError) Error() string {
	return w.msg
}

func NewWarningError(warningMsg string) *WarningError {
	return &WarningError{msg: warningMsg}
}
