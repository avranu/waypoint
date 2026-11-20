package core

import "database/sql"

type Service struct{ db *sql.DB }

func (s *Service) DB() *sql.DB { return s.db }
