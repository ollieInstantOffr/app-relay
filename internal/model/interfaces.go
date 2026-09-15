package model

import (
	"fmt"
	"sort"
	"strings"
)

// Validator is implemented by entities and settings that can check themselves.
// The generic CRUD API calls Validate before every create/update.
type Validator interface{ Validate() error }

// Redactor removes secrets before an entity leaves the API (called on a copy).
type Redactor interface{ Redact() }

// SecretKeeper prepares an entity for storage: hashes write-only plaintext
// fields and carries over secrets the client did not resend. prev is nil on
// create, otherwise a pointer of the same type.
type SecretKeeper interface{ KeepSecrets(prev any) error }

// ValidationError maps field paths ("domains.0", "upstream.port") to messages.
type ValidationError struct {
	Fields map[string]string `json:"fields"`
}

func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %s", k, e.Fields[k]))
	}
	return "invalid: " + strings.Join(parts, "; ")
}

// Errs accumulates field errors.
type Errs map[string]string

func (e Errs) Add(field, format string, args ...any) {
	if _, ok := e[field]; !ok {
		e[field] = fmt.Sprintf(format, args...)
	}
}

func (e Errs) Err() error {
	if len(e) == 0 {
		return nil
	}
	return &ValidationError{Fields: map[string]string(e)}
}
