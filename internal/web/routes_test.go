package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nextster/tg-radar/internal/config"
	"github.com/nextster/tg-radar/internal/monitor"
)

func TestRoutesAllowRootAndMCP(t *testing.T) {
	server := &Server{
		cfg:     config.Config{MCPToken: "secret"},
		monitor: &monitor.Service{},
	}
	handler := server.routes()

	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("POST /mcp status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestSplitTermGroups(t *testing.T) {
	got := splitTermGroups("USB-C, Type-C\n1000 lm, 1200 lm\r\n\n31.8")
	if len(got) != 3 || len(got[0]) != 2 || len(got[1]) != 2 || len(got[2]) != 1 {
		t.Fatalf("groups = %#v", got)
	}
}
