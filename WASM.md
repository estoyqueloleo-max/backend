# Guía de Depuración Go/WASM en Cloudflare Workers

Esta guía resume las lecciones aprendidas al migrar Pingo a Cloudflare Workers usando Go y WebAssembly.

## Errores Comunes y Soluciones

### 1. Error 1101 (Worker threw a JavaScript exception)
Este es el error más común. En el contexto de Go, suele significar:
- **Pánico en el código Go**: Algo ha causado un crash (ej. acceso a puntero nil).
- **Falta de Bindings**: El archivo `worker.mjs` no encuentra los bindings de KV o variables de entorno que el código Go intenta usar.
- **Incompatibilidad de Wrapper**: Estás usando un binario compilado con `Go` estándar pero el archivo `wasm_exec.js` es de `TinyGo` (o viceversa).

### 2. Código "Hung" (Colgado)
Ocurre cuando Cloudflare detecta que el Worker no devuelve una respuesta.
- **Causa en Go**: El runtime de Go está bloqueando el bucle de eventos.
- **Solución**: Evitar leer cuerpos de peticiones en rutas que no los tienen (como `OPTIONS` o `GET` accidentales). Asegurarse de que siempre se llama a `w.WriteHeader` o se escribe algo en el cuerpo.

## Estrategia de Depuración (Debug)

### Recuperación de Pánicos (Panic Recovery)
Implementar un `defer recover()` en el manejador principal para capturar errores internos y devolverlos como texto:

```go
func handler(w http.ResponseWriter, req *http.Request) {
    defer func() {
        if r := recover(); r != nil {
            w.Header().Set("Content-Type", "text/plain")
            w.WriteHeader(http.StatusInternalServerError)
            fmt.Fprintf(w, "Worker Panic: %v", r)
        }
    }()
    // ... tu lógica ...
}
```

### Logs en Tiempo Real
Usa `fmt.Printf` o `println` en Go. Para verlos:
1. Ejecuta `npx wrangler tail` en tu terminal.
2. O usa la pestaña **"Logs" -> "Real-time logs"** en el panel de Cloudflare.

## Compiladores: TinyGo vs Standard Go

| Característica | TinyGo | Standard Go (`GOOS=js`) |
| :--- | :--- | :--- |
| **Tamaño WASM** | Pequeño (~500KB - 1MB) | Grande (~5MB - 10MB) |
| **Estabilidad `net/http`** | Media (puede dar errores "Hung") | Muy Alta |
| **Reflexión** | Limitada | Completa |
| **Gorrutinas** | Limitadas (scheduler distinto) | Completas |

> [!IMPORTANT]
> Si cambias entre compiladores, **DEBES** regenerar los activos de JS:
> - Para Standard Go: `go run github.com/syumai/workers/cmd/workers-assets-gen -mode=go`
> - Para TinyGo: `go run github.com/syumai/workers/cmd/workers-assets-gen`

## Límites de Tamaño
- **Workers Free**: Límite de 1MB en el script final (comprimido).
- **Workers Paid**: Hasta 10MB+.
- Si tu `app.wasm` supera los 1MB, es probable que necesites usar Standard Go y tener una cuenta de pago, o optimizar mucho con TinyGo.

## Probando en Local
Usa siempre `npx wrangler dev` para probar antes de desplegar. Si funciona en local pero no en remoto, revisa que los IDs de los KV Namespaces en `wrangler.jsonc` sean correctos para el entorno de producción.
