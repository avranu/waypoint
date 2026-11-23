package core

// Package core composes the layers, so it imports them.
import (
	_ "corelayers/internal/core/forge"
	_ "corelayers/internal/core/kernel"
)

type Service struct{}
