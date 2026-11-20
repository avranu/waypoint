package api

import (
	"database/sql" // want `internal/api must not import database/sql`

	"apidb/internal/core"
)

var _ *sql.DB

func handler(s *core.Service) error {
	db := s.DB()
	return db.Ping() // want `internal/api must not use a sql.DB`
}
