// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package remote

import (
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elastic/elastic-agent-libs/transport/httpcommon"
)

func TestPackUnpack(t *testing.T) {
	c := Config{
		Protocol: Protocol("https"),
		SpaceID:  "123",
		Path:     "/ok",
		Transport: httpcommon.HTTPTransportSettings{
			Timeout: 10 * time.Second,
		},
	}

	b, err := yaml.Marshal(&c)
	require.NoError(t, err)

	c2 := Config{}

	err = yaml.Unmarshal(b, &c2)
	require.NoError(t, err)

	assert.True(t, reflect.DeepEqual(c, c2))
}
