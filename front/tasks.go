package main

import (
	"context"
	"errors"
	"iter"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// adoptingHandler lets kagent own task IDs. The kagent controller creates the
// task on its side and sends the first message with that task ID already set.
// a2a-go only accepts a message that names a task its own store already holds,
// so an unknown task is created here (submitted, in the message's context)
// before the request reaches the a2a-go handler.
type adoptingHandler struct {
	a2asrv.RequestHandler
	store taskstore.Store
}

func (h *adoptingHandler) adopt(ctx context.Context, req *a2a.SendMessageRequest) error {
	m := req.Message
	if m == nil || m.TaskID == "" {
		return nil
	}
	if _, err := h.store.Get(ctx, m.TaskID); !errors.Is(err, a2a.ErrTaskNotFound) {
		return err
	}
	_, err := h.store.Create(ctx, &a2a.Task{
		ID:        m.TaskID,
		ContextID: m.ContextID,
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	})
	if errors.Is(err, taskstore.ErrTaskAlreadyExists) {
		return nil
	}
	return err
}

func (h *adoptingHandler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	if err := h.adopt(ctx, req); err != nil {
		return nil, err
	}
	return h.RequestHandler.SendMessage(ctx, req)
}

func (h *adoptingHandler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	if err := h.adopt(ctx, req); err != nil {
		return func(yield func(a2a.Event, error) bool) { yield(nil, err) }
	}
	return h.RequestHandler.SendStreamingMessage(ctx, req)
}
