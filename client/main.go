package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var examples = []string{
	"Say hello in one sentence.",
	"Refactor this Python function to fix a race condition.",
	"Find today's weather in Bengaluru and summarize it.",
}

type routeResponse struct {
	Request         string  `json:"request"`
	LayaCheckpoint  string  `json:"laya_checkpoint"`
	TaskDifficulty  string  `json:"task_difficulty"`
	DifficultyScore float64 `json:"difficulty_score"`
	Domain          string  `json:"domain"`
	NeedsTools      float64 `json:"needs_tools"`
	Sensitive       float64 `json:"sensitive"`
	LLMDestination  string  `json:"llm_destination"`
	Reason          string  `json:"reason"`
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8080/route", "Go router API endpoint")
	flag.Parse()
	requests := flag.Args()
	if len(requests) == 0 {
		requests = examples
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	for _, request := range requests {
		body, _ := json.Marshal(map[string]string{"request": request})
		response, err := client.Post(*url, "application/json", bytes.NewReader(body))
		if err != nil {
			fmt.Fprintf(os.Stderr, "call Go server: %v\n", err)
			os.Exit(1)
		}
		if response.StatusCode != http.StatusOK {
			message, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
			response.Body.Close()
			fmt.Fprintf(os.Stderr, "server returned HTTP %d: %s\n", response.StatusCode, strings.TrimSpace(string(message)))
			os.Exit(1)
		}
		var result routeResponse
		err = json.NewDecoder(response.Body).Decode(&result)
		response.Body.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "decode server response: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\nRequest: %s\n", result.Request)
		fmt.Printf("Laya checkpoint: %s\n", result.LayaCheckpoint)
		fmt.Printf("Task difficulty: %s (%.2f/3)\n", result.TaskDifficulty, result.DifficultyScore)
		fmt.Printf("Signals: domain=%s, needs_tools=%.2f, sensitive=%.2f\n", result.Domain, result.NeedsTools, result.Sensitive)
		fmt.Printf("LLM destination: %s (%s)\n", result.LLMDestination, result.Reason)
	}
}
