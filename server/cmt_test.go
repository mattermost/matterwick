// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFilterNilInstances(t *testing.T) {
	inst := &E2EInstance{URL: "https://example.com", InstallationID: "id-1"}

	result := filterNilInstances([]*E2EInstance{inst, nil, inst})
	assert.Len(t, result, 2)

	assert.Len(t, filterNilInstances(nil), 0)
}
