# Standalone Go Laya router

This project contains a standalone Go server and a separate Go client for:

- Windows x64
- Apple Silicon macOS (`darwin/arm64`)
- Intel macOS (`darwin/amd64`)

The server embeds the native Laya inference engine and tokenizer for its target
platform. It does not require Python, PyTorch, Node.js, or a virtual environment.

## Run

Start the server from the project root:

```powershell
go run .\main.go
```

In another PowerShell window, run the client:

```powershell
go run .\client
go run .\client "Refactor this Python function to fix a race condition."
```

The first server launch downloads Laya's 324 MB multilingual ONNX model from
13 published npm chunks. Every chunk and the assembled model are checked with
SHA-256. Files are cached under `.cache/laya-native`; later launches work
offline. The native runtime and tokenizer are embedded in the Go executable.

To create normal executables:

```powershell
go build -o laya-router.exe .
go build -o laya-client.exe .\client
```

Then run `laya-router.exe` and `laya-client.exe` in separate terminals.

On macOS, use the equivalent commands:

```bash
go run .
go run ./client
```

To cross-compile Mac server binaries from Windows:

```powershell
$env:GOOS = "darwin"
$env:GOARCH = "arm64" # use amd64 for an Intel Mac
go build -o laya-router .
```

The server executable is about 178 MB because it includes the tokenizer and all
three native runtimes. Only the matching runtime is extracted and started.

## API

Send a request to `POST http://127.0.0.1:8080/route`:

```json
{"request":"Refactor this Python function to fix a race condition."}
```

The response includes Laya's domain and tool/sensitivity signals, a difficulty
classification of Low, Medium, or High, and the selected placeholder LLM
destination. Replace the values in `modelTiers` in `main.go` with your actual
provider model IDs.

`GET http://127.0.0.1:8080/health` reports whether the Go API and its embedded
native runtime started successfully. Stop the server with Ctrl+C.

## Memory requirements

Measured on Windows x64 with the multilingual model:

| Workload | Combined working memory |
| --- | ---: |
| Idle after model load | about 489 MB |
| Eight concurrent short requests | about 514 MB |
| Four concurrent requests of roughly 1,440 words each | about 1.6 GB observed |

The Go API itself used about 12 MB working memory; nearly all memory belongs to
the native model process. Reserve at least 1 GB of free RAM for ordinary short
requests. Reserve 2–4 GB when requests can be long or concurrent, or impose
request-length and concurrency limits. These numbers were measured on Windows;
the Mac builds compile successfully, but runtime memory still needs measuring
on the target Mac because the native runtime and OS memory accounting differ.

The server serializes model predictions by default (`--max-concurrent 1`) to
avoid the long-request spike seen in testing. Increase this flag only after
measuring the intended workload on the deployment machine. The 1.6 GB result
above came from an unbounded concurrency stress test.

## Runtime attribution

The embedded native runtime, tokenizer, and model are from the Apache-2.0
licensed `laya-system-one` community project, built on Laya by Convai
Innovations. Its license is embedded at `assets/LICENSE-laya-system-one.txt`.
Each compiled server includes all three native runtimes and selects the matching
one at startup.
