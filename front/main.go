// Command droid-harness serves kagent's BYO contract around Factory's Droid:
// A2A gRPC (lf.a2a.v1.A2AService) on runtime.port (default 80) and GET /readyz
// on 8081. Each turn runs `droid exec -o stream-json`.
//
// The front is Go because Substrate (gVisor) restores did not bring a Python
// process back: readyz was refused after every restore of a python:3.12 Actor,
// while a Go process in the same image restored in ~2s.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2agrpc "github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"google.golang.org/grpc"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("droid harness stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig(log)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("droid"); err != nil {
		return fmt.Errorf("droid CLI not found on PATH: %w", err)
	}
	if err := os.MkdirAll(cfg.Cwd, 0o755); err != nil {
		return err
	}

	port := cfg.Runtime.Port
	if v, err := strconv.Atoi(envOr("DROID_HARNESS_PORT", envOr("KAGENT_PORT", "0"))); err == nil && v > 0 {
		port = uint32(v)
	}
	if port == 0 {
		port = 80
	}
	readyzAddr := ":" + envOr("DROID_HARNESS_READYZ_PORT", "8081")

	card := agentCard(cfg)
	store := taskstore.NewInMemory(nil)
	handler := &adoptingHandler{
		RequestHandler: a2asrv.NewHandler(newDroidExecutor(cfg, log), a2asrv.WithTaskStore(store), a2asrv.WithExtendedAgentCard(card)),
		store:          store,
	}
	gs := grpc.NewServer()
	a2agrpc.NewHandler(handler).RegisterWith(gs)
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	readyz := &http.Server{Addr: readyzAddr, Handler: mux}

	errc := make(chan error, 2)
	go func() { errc <- gs.Serve(lis) }()
	go func() { errc <- readyz.ListenAndServe() }()
	log.Info("droid harness ready", "name", card.Name, "a2a_port", port, "readyz", readyzAddr,
		"model", cfg.Model, "autonomy", cfg.Autonomy, "mcp", cfg.MCPNames)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	gs.GracefulStop()
	return readyz.Shutdown(context.Background())
}

func agentCard(cfg *harnessConfig) *a2a.AgentCard {
	card := &a2a.AgentCard{}
	if len(cfg.AgentCard) > 0 {
		if err := json.Unmarshal(cfg.AgentCard, card); err != nil {
			card = &a2a.AgentCard{}
		}
	}
	if card.Name == "" {
		card.Name = envOr("KAGENT_NAME", "droid")
	}
	if card.Description == "" {
		card.Description = cfg.Agent.Description
	}
	if card.Version == "" {
		card.Version = "v1"
	}
	card.Capabilities.Streaming = true
	if len(card.DefaultInputModes) == 0 {
		card.DefaultInputModes = []string{"text"}
	}
	if len(card.DefaultOutputModes) == 0 {
		card.DefaultOutputModes = []string{"text"}
	}
	return card
}
