package core

type Service struct{}

var listedSchema = map[string]any{"type": "object"}

var missingSchema = map[string]any{"type": "object"}

func (s *Service) runChain(prompt string, schema map[string]any, out any) error { return nil }

func (s *Service) ask(schema map[string]any) error { return s.runChain("p", schema, nil) }

func (s *Service) A() error { return s.runChain("p", listedSchema, nil) }

func (s *Service) B() error { return s.runChain("p", missingSchema, nil) } // want `missingSchema is sent to a model but isn't in llmSchemas`

func (s *Service) C() error { return s.ask(missingSchema) } // want `missingSchema is sent to a model`

func (s *Service) D() error { return s.runChain("p", map[string]any{}, nil) } // want `isn't a package-level schema`

func (s *Service) E(listed bool) error {
	schema := listedSchema
	if !listed {
		schema = missingSchema
	}
	return s.runChain("p", schema, nil) // want `missingSchema is sent to a model`
}
