package runtime

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// stallingObserver stands in for teardown work (e.g. persistence) that only
// completes once the run context is cancelled: OnEvent parks on StreamStopped
// until ctx is done, so the channel cannot close before the consumer cancels.
type stallingObserver struct{ done chan struct{} }

func (stallingObserver) OnRunStart(context.Context, *session.Session) {}

func (o stallingObserver) OnEvent(ctx context.Context, _ *session.Session, event Event) {
	if _, ok := event.(*StreamStoppedEvent); ok {
		<-ctx.Done()
		close(o.done)
	}
}

// Run returning on the first ErrorEvent must not abandon the stream: it has
// to cancel the runtime it started and wait for the channel to close, so no
// teardown work outlives the call.
func TestLocalRuntime_RunCancelsAndDrainsStreamOnError(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		prov := &failingProvider{id: "test/failing", err: errors.New("401 unauthorized")}
		root := agent.New("root", "test", agent.WithModel(prov))
		obs := stallingObserver{done: make(chan struct{})}
		rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
			WithSessionCompaction(false),
			WithModelStore(mockModelStore{}),
			WithEventObserver(obs),
		)
		require.NoError(t, err)

		_, err = rt.Run(t.Context(), session.New(session.WithUserMessage("hi")))
		require.ErrorContains(t, err, "401 unauthorized")

		select {
		case <-obs.done:
		default:
			t.Fatal("Run returned before cancelling and draining its stream")
		}
	})
}

// stallingRemoteClient serves a stream that fails, waits for the caller to
// cancel, then floods more events than the runtime buffer holds. Only a
// consumer that cancels and drains lets the producer finish.
type stallingRemoteClient struct {
	stubRemoteClient

	done chan struct{}
}

func (c *stallingRemoteClient) RunAgent(ctx context.Context, _, _ string, _ []api.Message, _ string) (<-chan Event, error) {
	ch := make(chan Event)
	go func() {
		defer close(ch)
		defer close(c.done)
		ch <- Error("remote failure")
		<-ctx.Done()
		for range 2 * defaultEventChannelCapacity {
			ch <- Warning("trailing", "test")
		}
	}()
	return ch, nil
}

func (c *stallingRemoteClient) RunAgentWithAgentName(ctx context.Context, sessionID, agentFile, _ string, msgs []api.Message, model string) (<-chan Event, error) {
	return c.RunAgent(ctx, sessionID, agentFile, msgs, model)
}

func TestRemoteRuntime_RunCancelsAndDrainsStreamOnError(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		client := &stallingRemoteClient{
			stubRemoteClient: stubRemoteClient{cfg: &latest.Config{Agents: latest.Agents{{Name: "test"}}}},
			done:             make(chan struct{}),
		}
		rt, err := NewRemoteRuntime(client)
		require.NoError(t, err)

		_, err = rt.Run(t.Context(), session.New(session.WithUserMessage("hi")))
		require.EqualError(t, err, "remote failure")

		select {
		case <-client.done:
		default:
			t.Fatal("Run returned before cancelling and draining its stream")
		}
	})
}
