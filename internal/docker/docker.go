// Package docker drives the host's Docker daemon through the docker CLI.
//
// The CLI is used instead of the SDK because `docker build` brings BuildKit and
// its layer cache with it, which the SDK would require reimplementing.
package docker

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
)

// CLI runs Docker operations by invoking the docker command.
type CLI struct{}

// Container describes an app container to start.
type Container struct {
	Name    string
	Image   string
	Network string
	Env     map[string]string
	Labels  map[string]string
	Memory  string
	CPUs    float64
}

// Build builds the Dockerfile in dir and tags the image.
func (CLI) Build(ctx context.Context, dir, tag string, log io.Writer) error {
	return run(ctx, log, "build", "--progress=plain", "--tag", tag, dir)
}

// Test runs command in a throwaway container of image. The run is named so it
// can be removed if the deadline passes, since killing the client leaves it running.
func (c CLI) Test(ctx context.Context, name, image, command string, log io.Writer) error {
	err := run(ctx, log, "run", "--rm", "--name", name, "--entrypoint", "/bin/sh", image, "-c", command)
	if ctx.Err() != nil {
		_ = c.Remove(context.WithoutCancel(ctx), name)
	}
	return err
}

// Start runs a container detached. Docker restarts it if the app exits.
func (CLI) Start(ctx context.Context, c Container) error {
	args := []string{
		"run", "--detach",
		"--name", c.Name,
		"--network", c.Network,
		"--restart", "unless-stopped",
		"--memory", c.Memory,
		"--cpus", strconv.FormatFloat(c.CPUs, 'f', -1, 64),
	}
	for k, v := range c.Env {
		args = append(args, "--env", k+"="+v)
	}
	for k, v := range c.Labels {
		args = append(args, "--label", k+"="+v)
	}
	args = append(args, c.Image)
	return run(ctx, io.Discard, args...)
}

// Remove force-removes a container. A container that doesn't exist is not an error.
func (CLI) Remove(ctx context.Context, name string) error {
	return run(ctx, io.Discard, "rm", "--force", name)
}

func run(ctx context.Context, log io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = log
	// Keep stderr for the error message as well as the log.
	var tail tailBuffer
	cmd.Stderr = io.MultiWriter(log, &tail)
	if err := cmd.Run(); err != nil {
		if msg := tail.String(); msg != "" {
			return fmt.Errorf("docker %s: %w: %s", args[0], err, msg)
		}
		return fmt.Errorf("docker %s: %w", args[0], err)
	}
	return nil
}

// tailBuffer keeps the last few hundred bytes written to it.
type tailBuffer struct {
	data []byte
}

const tailLimit = 512

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.data = append(t.data, p...)
	if len(t.data) > tailLimit {
		t.data = t.data[len(t.data)-tailLimit:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	return string(t.data)
}
