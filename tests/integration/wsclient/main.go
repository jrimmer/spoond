package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// wsclient — integration-test WebSocket client for the forkd stream
// endpoint. Two modes:
//
//	stream:  send a command, expect incremental output frames + exit_code
//	stdin:   start bash, send stdin, expect roundtrip output
//
// Usage: wsclient -mode stream|stdin -url wss://host -token T -lease ID
//   -args "echo hi"  (stream mode: command string, shell-quoted)
func main() {
	mode := flag.String("mode", "stream", "stream|stdin")
	url := flag.String("url", "", "backend base URL, e.g. https://127.0.0.1:8890")
	token := flag.String("token", "", "consumer token")
	lease := flag.String("lease", "", "lease id")
	args := flag.String("args", "echo STREAM_OK", "command string for stream mode")
	pty := flag.Bool("pty", true, "allocate a PTY")
	flag.Parse()

	if *url == "" || *token == "" || *lease == "" {
		fmt.Fprintln(os.Stderr, "url/token/lease required")
		os.Exit(2)
	}

	wsURL := strings.TrimRight(*url, "/") + "/api/sandboxes/" + *lease + "/stream"
	wsURL = "wss://" + strings.TrimPrefix(strings.TrimPrefix(wsURL, "https://"), "http://")
	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	headers := map[string][]string{"Authorization": {"Bearer " + *token}}
	conn, _, err := dialer.Dial(wsURL, headers)
	if err != nil {
		fmt.Println("DIAL ERR:", err)
		os.Exit(1)
	}
	defer conn.Close()
	fmt.Println("WS_CONNECTED")

	switch *mode {
	case "stream":
		req, _ := json.Marshal(map[string]any{
			"args": []string{"bash", "-c", *args},
			"pty":  *pty,
		})
		if err := conn.WriteMessage(websocket.TextMessage, req); err != nil {
			fmt.Println("WRITE ERR:", err)
			os.Exit(1)
		}
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				fmt.Println("READ DONE:", err)
				break
			}
			if mt == websocket.TextMessage {
				fmt.Printf("RECV: %s", msg)
				if strings.Contains(string(msg), `"exit_code"`) {
					fmt.Println("STREAM_COMPLETE")
					return
				}
			}
		}
		fmt.Println("STREAM_TIMEOUT")
		os.Exit(1)

	case "stdin":
		req, _ := json.Marshal(map[string]any{"args": []string{"bash"}, "pty": *pty})
		if err := conn.WriteMessage(websocket.TextMessage, req); err != nil {
			fmt.Println("WRITE ERR:", err)
			os.Exit(1)
		}
		time.Sleep(1 * time.Second)
		msg, _ := json.Marshal(map[string]string{"in": "echo STDIN_ROUNDTRIP_OK; exit\n"})
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			fmt.Println("WRITE ERR:", err)
			os.Exit(1)
		}
		deadline := time.Now().Add(10 * time.Second)
		got := false
		for time.Now().Before(deadline) {
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			mt, data, err := conn.ReadMessage()
			if err != nil {
				break
			}
			if mt == websocket.TextMessage {
				fmt.Printf("RECV: %s", data)
				if strings.Contains(string(data), "STDIN_ROUNDTRIP_OK") {
					got = true
				}
				if strings.Contains(string(data), `"exit_code"`) {
					break
				}
			}
		}
		if got {
			fmt.Println("STDIN_ROUNDTRIP_OK")
			return
		}
		fmt.Println("STDIN_ROUNDTRIP_FAILED")
		os.Exit(1)
	}
}
