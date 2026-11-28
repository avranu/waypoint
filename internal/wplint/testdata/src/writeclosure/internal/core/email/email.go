// Package email is the mail sender under internal/core.
package email

import "context"

type Mailer struct{}

func (m *Mailer) Send(ctx context.Context, to, subject, body string) error { return nil }
