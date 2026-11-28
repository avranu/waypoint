// Package dom is a domain under internal/core.
package dom

type Service struct{}

var listedSchema = map[string]any{"type": "object"}

var missingSchema = map[string]any{"type": "object"}

func (s *Service) runChain(prompt string, schema map[string]any, out any) error { return nil }

func (s *Service) A() error { return s.runChain("p", listedSchema, nil) }

func (s *Service) B() error { return s.runChain("p", missingSchema, nil) } // want `missingSchema is sent to a model but isn't in llmSchemas`
