// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

// Code generated by /dev-tools/cmd/buildspec/buildPgp.go - DO NOT EDIT.

package release

import (
	"github.com/elastic/elastic-agent/pkg/packer"
)

// pgp bytes is a packed in public gpg key
var pgpBytes []byte

func init() {
	// Packed Files
	// GPG-KEY-elasticsearch
	pgpBytes = packer.MustUnpack("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")["GPG-KEY-elasticsearch"]
}

// PGP return pgpbytes.
func PGP() []byte {
	return pgpBytes
}
