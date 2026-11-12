package cli

import "encoding/json"

type artifact struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Type   string `json:"type"`
	Goos   string `json:"goos"`
	Goarch string `json:"goarch"`
	Extra  struct {
		Format   any `json:"Format"`
		Checksum any `json:"Checksum"`
	} `json:"extra"`

	fields map[string]json.RawMessage
}
