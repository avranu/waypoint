package core

import "context"

type Actor struct{ ID int64 }

type Service struct{}

func (s *Service) Get(ctx context.Context, actor *Actor, id int64) error { // want `Get takes an \*Actor and never reads it`
	return nil
}

func (s *Service) Drop(ctx context.Context, _ *Actor) error { // want `Drop takes an \*Actor and drops it`
	return nil
}

func (s *Service) Unnamed(context.Context, *Actor) error { // want `Unnamed takes an \*Actor and drops it`
	return nil
}

func (s *Service) Checked(ctx context.Context, actor *Actor) error {
	if actor == nil {
		return nil
	}
	return nil
}

func (s *Service) unexported(actor *Actor) {}
