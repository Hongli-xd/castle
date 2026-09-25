package oci

import (
	"sandcastle.dev/sandcastle/internal/apperr"
)

// asAppErr unwraps errors produced by this package's client helpers.
func asAppErr(err error) (*apperr.E, bool) {
	if err == nil {
		return nil, false
	}
	if ae, ok := err.(*apperr.E); ok {
		return ae, true
	}
	return nil, false
}
