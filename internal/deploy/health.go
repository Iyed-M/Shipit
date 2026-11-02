package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/Iyed-M/Shipit/internal/caddy"
	"github.com/Iyed-M/Shipit/internal/docker"
)

const (
	// healthPasses is how many checks in a row must pass, so an app that
	// answers once and then falls over doesn't get traffic.
	healthPasses = 3
	probeTimeout = 2 * time.Second
)

// Prober checks once whether an app answers.
type Prober interface {
	// Probe GETs path at addr with the given Host header, needing a 2xx. With no
	// path it only checks that addr accepts connections.
	Probe(ctx context.Context, addr, host, path string) error
}

// NetworkProber reaches apps over their Docker network. Shipit must share that
// network with them.
type NetworkProber struct{}

// Probe implements Prober.
func (NetworkProber) Probe(ctx context.Context, addr, host, path string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	if path == "" {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return conn.Close()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return err
	}
	req.Host = host
	client := &http.Client{
		// A redirect isn't a 2xx, and following one could leave the container.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s returned %s", path, resp.Status)
	}
	return nil
}

// waitHealthy polls the container until enough checks pass in a row. A
// container that exits fails at once: waiting out the timeout would only delay
// the verdict.
func (p *Pipeline) waitHealthy(ctx context.Context, name, host, path string) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timing.HealthTimeout)
	defer cancel()

	var (
		passes int
		last   error
	)
	for {
		state, err := p.runtime.Inspect(ctx, name)
		if err != nil {
			return p.healthFailure(ctx, err)
		}
		if crashed(state) {
			return crashError(state)
		}

		addr := net.JoinHostPort(state.IP, strconv.Itoa(caddy.AppPort))
		if last = p.prober.Probe(ctx, addr, host, path); last == nil {
			passes++
			if passes == healthPasses {
				return nil
			}
		} else {
			passes = 0
		}

		select {
		case <-ctx.Done():
			if last == nil {
				last = ctx.Err()
			}
			return p.healthFailure(ctx, last)
		case <-time.After(p.cfg.Timing.HealthInterval):
		}
	}
}

func crashed(s docker.State) bool {
	return !s.Running || s.Restarting || s.RestartCount > 0
}

func crashError(s docker.State) error {
	switch {
	case s.OOMKilled:
		return errors.New("the app ran out of memory during the health check")
	case s.Restarting || !s.Running:
		return fmt.Errorf("the app exited with code %d during the health check", s.ExitCode)
	default:
		return errors.New("the app crashed during the health check")
	}
}

// healthFailure says the check ran out of time, if that is what happened.
func (p *Pipeline) healthFailure(ctx context.Context, cause error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("health check timed out after %s: %w", p.cfg.Timing.HealthTimeout, cause)
	}
	return cause
}
