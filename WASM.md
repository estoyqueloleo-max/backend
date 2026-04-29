# Go/WASM Debugging Guide for Cloudflare Workers

This guide summarizes the lessons learned while migrating P2PT to Cloudflare Workers using Go and WebAssembly.

## Common Errors and Solutions

### 1. Error 1101 (Worker threw a JavaScript exception)
This is the most common error. In the Go context, it usually means:
- **Go code panic**: Something caused a crash (e.g., nil pointer dereference).
- **Missing Bindings**: The `worker.mjs` file cannot find the KV bindings or environment variables that the Go code attempts to use.
- **Wrapper Incompatibility**: You are using a binary compiled with standard `Go` but the `wasm_exec.js` file is from `TinyGo` (or vice versa).

### 2. "Hung" Code
Occurs when Cloudflare detects that the Worker is not returning a response.
- **Cause in Go**: The Go runtime is blocking the event loop.
- **Solution**: Avoid reading request bodies in routes that don't have them (like `OPTIONS` or accidental `GET`s). Ensure that `w.WriteHeader` is always called or something is written to the body.

## Debugging Strategy

### Panic Recovery
Implement a `defer recover()` in the main handler to capture internal errors and return them as text:

```go
func handler(w http.ResponseWriter, req *http.Request) {
    defer func() {
        if r := recover(); r != nil {
            w.Header().Set("Content-Type", "text/plain")
            w.WriteHeader(http.StatusInternalServerError)
            fmt.Fprintf(w, "Worker Panic: %v", r)
        }
    }()
    // ... your logic ...
}
```

### Real-time Logs
Use `fmt.Printf` or `println` in Go. To view them:
1. Run `npx wrangler tail` in your terminal.
2. Or use the **"Logs" -> "Real-time logs"** tab in the Cloudflare dashboard.

## Compilers: TinyGo vs Standard Go

| Feature | TinyGo | Standard Go (`GOOS=js`) |
| :--- | :--- | :--- |
| **WASM Size** | Small (~500KB - 1MB) | Large (~5MB - 10MB) |
| **`net/http` Stability** | Medium (may cause "Hung" errors) | Very High |
| **Reflection** | Limited | Full |
| **Goroutines** | Limited (different scheduler) | Full |

> [!IMPORTANT]
> If you switch between compilers, you **MUST** regenerate the JS assets:
> - For Standard Go: `go run github.com/syumai/workers/cmd/workers-assets-gen -mode=go`
> - For TinyGo: `go run github.com/syumai/workers/cmd/workers-assets-gen`

## Size Limits
- **Free Workers**: 1MB limit on the final script (compressed).
- **Paid Workers**: Up to 10MB+.
- If your `app.wasm` exceeds 1MB, you will likely need to use Standard Go and have a paid account, or optimize heavily with TinyGo.

## Local Testing
Always use `npx wrangler dev` to test before deploying. If it works locally but not remotely, check that the KV Namespace IDs in `wrangler.jsonc` are correct for the production environment.
