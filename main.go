package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

const (
	modelSHA256 = "dae83372e835bdc9949dae9bb6663c7d727101d2878f3fa8a66fe9869f8bc269"
	modelSize   = int64(324125608)
)

type modelChunk struct {
	Size   int64
	SHA256 string
}

var modelChunks = []modelChunk{
	{25165824, "ef5fb331bbc913691ddb94c65efd4139bbad581870f00ceec697e27a1a277d68"},
	{25165824, "197cf82966fa114b1c5480d67dfb0c69e2a29b26d9cdd260a15d34ed7e94bc3d"},
	{25165824, "a72e60a93dfa4e93eedc45eb0bfc0c0ab48010db35d93acd613c2312e59c35e1"},
	{25165824, "7db3119f56cc6a4874b0c300effc06ae0f2e4587486d4c6673d74375157426b6"},
	{25165824, "b3c66e9ae3210f4a6b0e89e235e880b9eea6636afe33c90e21b07c4260dfea91"},
	{25165824, "eb659ce78d4deb34107eff20b86339d77f5fd4e2ae3ccad625775528cd376b6c"},
	{25165824, "9ac3fd26e588debb86e2368093e58272b9a318515b9eaebf07da8d1429dc5b62"},
	{25165824, "40c158c978ba8285a69b41282308f108d24390734660935be69c6b58ea186b6a"},
	{25165824, "d8ce88b2cc1c2f0bcb6f50414fb56741a21d3d2221db19c9fff8879f5e58668e"},
	{25165824, "a83210739f94a9ffbf99c32aedd6e8ac68bcbe8be42d57485fff9534e2d8011b"},
	{25165824, "c06ed434512a4a8d610d27f52a91bdfff46001eab2b586f11d65c506140753f0"},
	{25165824, "44a0a9ad824ab26b8a1c5edb2d71c7283341e4bee5e1bfa4de2a4a9a813e9081"},
	{22135720, "35e2c872c113e76456e6790f08c9714f2b83f8a049a550814efd5355640df5ce"},
}

//go:embed assets/*
var runtimeFiles embed.FS

type embeddedAsset struct {
	Files  fs.FS
	Source string
	Target string
	Size   int64
	SHA256 string
	Mode   os.FileMode
}

var commonAssets = []embeddedAsset{
	{runtimeFiles, "assets/tokenizer.json", "tokenizer.json", 34363188, "609d8f4c067cd3950f88594c5a802616cea245823836ef5848ee4fc40aab5b6f", 0o644},
	{runtimeFiles, "assets/rl_agent_config.json", "rl_agent_config.json", 491, "5dcc5e459d2468c4e407f70fa66475c5ae01fa2ca4f41ba1f55b43c82508bca2", 0o644},
	{runtimeFiles, "assets/LICENSE-laya-system-one.txt", "LICENSE-laya-system-one.txt", 10333, "4c0d61590e0aa1a3297fba604cb84e394f119a408dc944479cc1e4ed75f3ccc3", 0o644},
}

type answer struct {
	Choice string  `json:"choice"`
	Score  float64 `json:"score"`
	Noul   float64 `json:"noul"`
}

type prediction struct {
	Model   string            `json:"model"`
	Answers map[string]answer `json:"answers"`
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

var routerQuestions = map[string]any{
	"difficulty": map[string]any{
		"type": "score", "instructions": "How hard is `request` for a language model?",
		"criteria": []string{
			"trivial: a lookup or one-liner",
			"easy: short answer, no reasoning",
			"moderate: several steps",
			"hard: long multi-step reasoning or specialist knowledge",
		},
	},
	"domain": map[string]any{
		"type": "choice", "instructions": "What domain does `request` belong to?",
		"criteria": map[string]string{
			"code":           "software engineering, programming, refactoring, architecture, debugging",
			"math_or_logic":  "mathematics, logic puzzles, proofs, complex calculation",
			"writing":        "creative writing, essays, emails, blog posts, copywriting",
			"factual_lookup": "facts, definitions, trivia, history",
			"data_analysis":  "statistics, SQL, data manipulation, metrics",
			"chitchat":       "casual conversation, greetings, small talk",
		},
	},
	"needs_tools": map[string]any{
		"type": "noul", "instructions": "Does answering `request` require external tools, search or private data?",
	},
	"is_sensitive": map[string]any{
		"type": "noul", "instructions": "Does `request` involve money, legal, medical or safety consequences?",
	},
}

var currentDataPattern = regexp.MustCompile(`(?i)\b(today|latest|current|weather|search the web)\b`)

var modelTiers = map[string]string{
	"fast": "fast-model", "coding": "coding-model",
	"reasoning": "reasoning-model", "tools": "tool-enabled-model",
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "Go API listen address")
	cacheDir := flag.String("cache-dir", defaultCacheDir(), "directory for the extracted runtime and model")
	maxConcurrent := flag.Int("max-concurrent", 1, "maximum simultaneous Laya predictions")
	flag.Parse()
	if *maxConcurrent < 1 {
		log.Fatal("--max-concurrent must be at least 1")
	}

	runtimeDir, err := filepath.Abs(*cacheDir)
	if err != nil {
		log.Fatalf("resolve runtime cache: %v", err)
	}
	binary, err := prepareRuntime(runtimeDir)
	if err != nil {
		log.Fatal(err)
	}
	debug.FreeOSMemory()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	native, err := startNative(ctx, runtimeDir, binary)
	if err != nil {
		log.Fatal(err)
	}
	defer native.close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "runtime": "native-laya"})
	})
	mux.HandleFunc("POST /route", routeHandler(native, make(chan struct{}, *maxConcurrent)))

	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("standalone Go Laya router listening on http://%s", *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func defaultCacheDir() string {
	return filepath.Join(".cache", "laya-native")
}

func prepareRuntime(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create runtime cache: %w", err)
	}
	assets, binaryName, err := platformAssets()
	if err != nil {
		return "", err
	}
	assets = append(commonAssets, assets...)
	for _, asset := range assets {
		if err := ensureEmbeddedAsset(dir, asset); err != nil {
			return "", err
		}
	}
	if err := ensureModel(dir); err != nil {
		return "", err
	}
	return filepath.Join(dir, binaryName), nil
}

func ensureEmbeddedAsset(dir string, asset embeddedAsset) error {
	path := filepath.Join(dir, asset.Target)
	marker := path + ".sha256"
	if info, err := os.Stat(path); err == nil && info.Size() == asset.Size {
		if digest, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(digest)) == asset.SHA256 {
			return nil
		}
		if ok, err := verifyFile(path, asset.SHA256); err == nil && ok {
			return os.WriteFile(marker, []byte(asset.SHA256+"\n"), 0o644)
		}
	}
	source, err := asset.Files.Open(asset.Source)
	if err != nil {
		return fmt.Errorf("open embedded %s: %w", asset.Source, err)
	}
	defer source.Close()
	tmp := path + ".tmp"
	destination, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, asset.Mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", asset.Target, err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(destination, hash), source)
	closeErr := destination.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %v %v", asset.Target, copyErr, closeErr)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if written != asset.Size || digest != asset.SHA256 {
		_ = os.Remove(tmp)
		return fmt.Errorf("embedded %s failed verification", asset.Source)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install %s: %w", asset.Target, err)
	}
	return os.WriteFile(marker, []byte(asset.SHA256+"\n"), 0o644)
}

func platformAssets() ([]embeddedAsset, string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return []embeddedAsset{
			{runtimeFiles, "assets/laya-serve-windows-amd64.exe", "laya-serve.exe", 26848768, "89dfb9e77dcdaeb8d4646720c7b5bdb4d278119013f195e8b7561659ef0b775a", 0o755},
		}, "laya-serve.exe", nil
	case "darwin/arm64":
		return []embeddedAsset{
			{runtimeFiles, "assets/laya-serve-darwin-arm64", "laya-serve", 22366320, "68a477ae9d4f852256d5ed10ae97bae19c98a3649da11c3c3058d635dac442d1", 0o755},
		}, "laya-serve", nil
	case "darwin/amd64":
		return []embeddedAsset{
			{runtimeFiles, "assets/laya-serve-darwin-amd64", "laya-serve", 4869416, "7a2b16f089a8186a480ee2de05849435150d486a3947dcf530f02b316bfe0558", 0o755},
			{runtimeFiles, "assets/libonnxruntime.1.23.0-darwin-amd64.dylib", "libonnxruntime.1.23.0.dylib", 39582416, "42953d91536da6f34f8d4aae9188ae4a199ef689c61505f7d0cf73b611799aac", 0o755},
			{runtimeFiles, "assets/libonnxruntime-darwin-amd64.dylib", "libonnxruntime.dylib", 39582416, "3fcaaea7f91fd4e6eee577298157fca63d26fe039fd7aab18068f018bc498f74", 0o755},
		}, "laya-serve", nil
	default:
		return nil, "", fmt.Errorf("no embedded Laya runtime for %s/%s; supported: windows/amd64, darwin/arm64, darwin/amd64", runtime.GOOS, runtime.GOARCH)
	}
}

func ensureModel(dir string) error {
	path := filepath.Join(dir, "model.onnx")
	marker := path + ".sha256"
	if info, err := os.Stat(path); err == nil && info.Size() == modelSize {
		if digest, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(digest)) == modelSHA256 {
			return nil
		}
		if ok, err := verifyFile(path, modelSHA256); err == nil && ok {
			return os.WriteFile(marker, []byte(modelSHA256+"\n"), 0o644)
		}
	}

	log.Printf("downloading the Laya model once (%.0f MB in %d verified chunks)...", float64(modelSize)/1e6, len(modelChunks))
	client := &http.Client{Timeout: 30 * time.Minute}
	tmp := path + ".partial"
	file, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create model file: %w", err)
	}
	hash := sha256.New()
	var written int64
	for index, chunk := range modelChunks {
		count, err := downloadChunk(client, index, chunk, io.MultiWriter(file, hash))
		if err != nil {
			_ = file.Close()
			_ = os.Remove(tmp)
			return err
		}
		written += count
		log.Printf("downloaded Laya model chunk %d/%d", index+1, len(modelChunks))
	}
	closeErr := file.Close()
	if closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("save Laya model: %w", closeErr)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if written != modelSize || digest != modelSHA256 {
		_ = os.Remove(tmp)
		return fmt.Errorf("downloaded model failed verification (bytes=%d, sha256=%s)", written, digest)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install Laya model: %w", err)
	}
	if err := os.WriteFile(marker, []byte(modelSHA256+"\n"), 0o644); err != nil {
		return err
	}
	log.Printf("Laya model is ready")
	return nil
}

func downloadChunk(client *http.Client, index int, expected modelChunk, destination io.Writer) (int64, error) {
	name := fmt.Sprintf("laya-model-chunk-%02d", index)
	url := fmt.Sprintf("https://registry.npmjs.org/@sys-one/%s/-/%s-1.1.0.tgz", name, name)
	response, err := client.Get(url)
	if err != nil {
		return 0, fmt.Errorf("download model chunk %d: %w", index, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download model chunk %d: HTTP %d", index, response.StatusCode)
	}
	gzipReader, err := gzip.NewReader(response.Body)
	if err != nil {
		return 0, fmt.Errorf("open model chunk %d: %w", index, err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read model chunk %d: %w", index, err)
		}
		if filepath.ToSlash(header.Name) != "package/chunk.bin" {
			continue
		}
		chunkHash := sha256.New()
		written, err := io.Copy(io.MultiWriter(destination, chunkHash), tarReader)
		if err != nil {
			return 0, fmt.Errorf("extract model chunk %d: %w", index, err)
		}
		digest := hex.EncodeToString(chunkHash.Sum(nil))
		if written != expected.Size || digest != expected.SHA256 {
			return 0, fmt.Errorf("model chunk %d failed verification (bytes=%d, sha256=%s)", index, written, digest)
		}
		return written, nil
	}
	return 0, fmt.Errorf("model chunk %d did not contain package/chunk.bin", index)
}

func verifyFile(path, expected string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false, err
	}
	return hex.EncodeToString(hash.Sum(nil)) == expected, nil
}

type nativeServer struct {
	cmd     *exec.Cmd
	baseURL string
	client  *http.Client
}

func startNative(ctx context.Context, dir, binary string) (*nativeServer, error) {
	cmd := exec.CommandContext(ctx, binary,
		"--model-dir", dir,
		"--host", "127.0.0.1",
		"--port", "0",
		"--threads", "0",
	)
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start embedded Laya runtime: %w", err)
	}

	ready := make(chan string, 1)
	scanErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "LAYA_READY ") {
				ready <- strings.TrimSpace(strings.TrimPrefix(line, "LAYA_READY "))
				io.Copy(io.Discard, stdout)
				return
			}
		}
		scanErr <- scanner.Err()
	}()

	select {
	case address := <-ready:
		parts := strings.Split(address, ":")
		port := parts[len(parts)-1]
		return &nativeServer{
			cmd: cmd, baseURL: "http://127.0.0.1:" + port,
			client: &http.Client{Timeout: 2 * time.Minute},
		}, nil
	case err := <-scanErr:
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("embedded Laya runtime exited before ready: %v: %s", err, strings.TrimSpace(stderr.String()))
	case <-time.After(2 * time.Minute):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("embedded Laya runtime timed out: %s", strings.TrimSpace(stderr.String()))
	}
}

func (server *nativeServer) close() {
	if server == nil || server.cmd == nil || server.cmd.Process == nil {
		return
	}
	_ = server.cmd.Process.Kill()
	_ = server.cmd.Wait()
}

func (server *nativeServer) predict(ctx context.Context, request string) (prediction, error) {
	var result prediction
	payload, err := json.Marshal(map[string]any{
		"model":     "laya-multilingual",
		"state":     map[string]string{"request": request},
		"questions": routerQuestions,
	})
	if err != nil {
		return result, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, server.baseURL+"/v1/systemone", bytes.NewReader(payload))
	if err != nil {
		return result, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := server.client.Do(httpRequest)
	if err != nil {
		return result, fmt.Errorf("call embedded Laya runtime: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return result, fmt.Errorf("Laya returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return result, fmt.Errorf("decode Laya response: %w", err)
	}
	for _, key := range []string{"difficulty", "domain", "needs_tools", "is_sensitive"} {
		if _, ok := result.Answers[key]; !ok {
			return result, fmt.Errorf("Laya response is missing %q", key)
		}
	}
	return result, nil
}

func routeHandler(native *nativeServer, slots chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		var input struct {
			Request string `json:"request"`
		}
		request.Body = http.MaxBytesReader(w, request.Body, 64<<10)
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil || strings.TrimSpace(input.Request) == "" {
			http.Error(w, "expected JSON with a nonempty request", http.StatusBadRequest)
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			http.Error(w, "expected one JSON object", http.StatusBadRequest)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-request.Context().Done():
			return
		}

		result, err := native.predict(request.Context(), input.Request)
		if err != nil {
			log.Printf("Laya prediction failed: %v", err)
			http.Error(w, "Laya prediction failed", http.StatusBadGateway)
			return
		}
		answers := result.Answers
		tier, reason := chooseTier(input.Request, answers)
		writeJSON(w, http.StatusOK, routeResponse{
			Request: input.Request, LayaCheckpoint: result.Model,
			TaskDifficulty:  difficultyLabel(answers["difficulty"].Score),
			DifficultyScore: answers["difficulty"].Score,
			Domain:          answers["domain"].Choice, NeedsTools: answers["needs_tools"].Noul,
			Sensitive:      answers["is_sensitive"].Noul,
			LLMDestination: modelTiers[tier], Reason: reason,
		})
	}
}

func difficultyLabel(score float64) string {
	switch {
	case score < 1.5:
		return "Low"
	case score < 2.5:
		return "Medium"
	default:
		return "High"
	}
}

func chooseTier(request string, answers map[string]answer) (string, string) {
	switch {
	case answers["needs_tools"].Noul >= 0.75 || currentDataPattern.MatchString(request):
		return "tools", "request needs external tools or current data"
	case answers["is_sensitive"].Noul >= 0.5 || answers["difficulty"].Score >= 2:
		return "reasoning", "sensitive or multi-step request"
	case answers["domain"].Choice == "code":
		return "coding", "software development request"
	default:
		return "fast", "short or straightforward request"
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
