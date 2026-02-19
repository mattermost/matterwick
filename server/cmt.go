// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import "encoding/json"

// marshalToJSON marshals data to JSON string
func marshalToJSON(data interface{}) (string, error) {
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	return string(jsonBytes), nil
}

// filterNilInstances returns a new slice with nil entries removed
func filterNilInstances(instances []*E2EInstance) []*E2EInstance {
	var result []*E2EInstance
	for _, inst := range instances {
		if inst != nil {
			result = append(result, inst)
		}
	}
	return result
}
