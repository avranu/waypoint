//go:build !linux

package main

import "context"

// The sandbox escape drill runs only where there's a sandbox helper: Linux
// (runnerFeatureList doesn't offer it elsewhere).
func (p *runnerProc) execSandboxDrill(context.Context, *jobRun, *claimedJob) outcome {
	return failed("the sandbox escape drill runs only on Linux")
}
