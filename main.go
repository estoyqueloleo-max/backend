package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/rs/cors"
	"github.com/syumai/workers"
	"github.com/syumai/workers/cloudflare"
	"github.com/syumai/workers/cloudflare/kv"
)

const (
	kvNamespace     = "PINGO_AUTH"
	vapidPublicKey  = "BEJ45uzzL_hw2MpJaxTw8Jwk-hbqJE3D5GI7TWMBaYOLkKoVsJQJGVZrpDOASMBpsCpF3bFI2LFZaZecqAWfAKk"
	vapidPrivateKey = "uwHBVq5KHmevNRtVBTP62G_n6mFeH7vOQsVlpG_YHWU"
	subscriberEmail = "admin@accreativos.com"
)

func generateAuthToken(salt string) string {
	if salt == "" {
		return "public"
	}
	now := time.Now().UTC()
	timeKey := fmt.Sprintf("%d-%d-%d-%d", now.Year(), int(now.Month())-1, now.Day(), now.Hour())
	message := salt + timeKey
	hash := sha256.Sum256([]byte(message))
	return fmt.Sprintf("%x", hash)
}

func checkAuth(req *http.Request, userPubKey string) (bool, string) {
	pingoKV, err := kv.NewNamespace(kvNamespace)
	if err != nil {
		return false, "KV Init Error"
	}
	userDataStr, err := pingoKV.GetString(userPubKey, nil)
	if err != nil || userDataStr == "" {
		return false, "User not authorized"
	}
	var userData struct {
		Salt string `json:"salt"`
	}
	json.Unmarshal([]byte(userDataStr), &userData)
	clientToken := req.Header.Get("X-Pingo-Auth")
	if clientToken == "" {
		return false, "Missing Auth Token"
	}
	if clientToken == generateAuthToken(userData.Salt) {
		return true, ""
	}
	return false, "Invalid Auth Token"
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				fmt.Fprintf(os.Stderr, "[PANIC] %v\n", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("Pingo Cloud is active"))
	})

	mux.HandleFunc("/auth/register", func(w http.ResponseWriter, req *http.Request) {
		var payload struct {
			UserPubKey string `json:"userPublicKey"`
			Salt       string `json:"salt"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			http.Error(w, "Invalid Payload", http.StatusBadRequest)
			return
		}
		pingoKV, _ := kv.NewNamespace(kvNamespace)
		data, _ := json.Marshal(payload)
		pingoKV.PutString(payload.UserPubKey, string(data), nil)
		fmt.Fprintf(os.Stderr, "[Auth] Registered: %s\n", payload.UserPubKey)
		w.Write([]byte("User registered"))
	})

	mux.HandleFunc("/turn-credentials", func(w http.ResponseWriter, req *http.Request) {
		userPubKey := req.URL.Query().Get("peerId")
		if ok, msg := checkAuth(req, userPubKey); !ok {
			http.Error(w, msg, http.StatusUnauthorized)
			return
		}
		turnURL := cloudflare.Getenv("TURN_URL")
		turnUser := cloudflare.Getenv("TURN_USERNAME")
		turnCred := cloudflare.Getenv("TURN_CREDENTIAL")
		turnSecret := cloudflare.Getenv("TURN_STATIC_AUTH_SECRET")

		if turnUser != "" && turnCred != "" {
			json.NewEncoder(w).Encode(map[string]any{
				"iceServers": []map[string]any{
					{"urls": []string{turnURL}, "username": turnUser, "credential": turnCred},
				},
			})
			return
		}

		if turnSecret != "" {
			timestamp := time.Now().Unix() + 3600
			username := fmt.Sprintf("%d:%s", timestamp, userPubKey)
			mac := hmac.New(sha1.New, []byte(turnSecret))
			mac.Write([]byte(username))
			password := base64.StdEncoding.EncodeToString(mac.Sum(nil))
			json.NewEncoder(w).Encode(map[string]any{
				"iceServers": []map[string]any{
					{"urls": []string{turnURL}, "username": username, "credential": password},
				},
			})
			return
		}
		http.Error(w, fmt.Sprintf("TURN config missing (URL:%s, User:%s, CredLen:%d)", turnURL, turnUser, len(turnCred)), http.StatusInternalServerError)
	})

	mux.HandleFunc("/push/subscribe", func(w http.ResponseWriter, req *http.Request) {
		var subscription map[string]any
		if err := json.NewDecoder(req.Body).Decode(&subscription); err != nil {
			http.Error(w, "Invalid Payload", http.StatusBadRequest)
			return
		}
		userPubKey, _ := subscription["userPublicKey"].(string)
		salt, _ := subscription["salt"].(string)
		if userPubKey == "" {
			http.Error(w, "UserPubKey required", http.StatusBadRequest)
			return
		}
		pingoKV, _ := kv.NewNamespace(kvNamespace)
		subscriptionStr, _ := json.Marshal(subscription)
		pingoKV.PutString("push:"+userPubKey, string(subscriptionStr), nil)
		if salt != "" {
			authPayload := map[string]string{"userPublicKey": userPubKey, "salt": salt}
			authData, _ := json.Marshal(authPayload)
			pingoKV.PutString(userPubKey, string(authData), nil)
		}
		fmt.Fprintf(os.Stderr, "[Push] Subscribed: %s\n", userPubKey)
		w.Write([]byte("Cloud activated"))
	})

	mux.HandleFunc("/push/send/", func(w http.ResponseWriter, req *http.Request) {
		targetId := strings.TrimPrefix(req.URL.Path, "/push/send/")
		senderId := req.URL.Query().Get("from")
		fmt.Fprintf(os.Stderr, "[Push] Relay attempt from %s to %s\n", senderId, targetId)

		if ok, msg := checkAuth(req, senderId); !ok {
			fmt.Fprintf(os.Stderr, "[Push] Auth failed for %s: %s\n", senderId, msg)
			http.Error(w, msg, http.StatusUnauthorized)
			return
		}

		pingoKV, _ := kv.NewNamespace(kvNamespace)
		subStr, err := pingoKV.GetString("push:"+targetId, nil)
		if err != nil || subStr == "" {
			fmt.Fprintf(os.Stderr, "[Push] Target %s not found in KV\n", targetId)
			http.Error(w, "Target offline or not subscribed", http.StatusNotFound)
			return
		}

		s := &webpush.Subscription{}
		// Clean up common non-json values that might be in KV
		if subStr == "null" || subStr == "<null>" || subStr == "" {
			fmt.Fprintf(os.Stderr, "[Push] Target %s has invalid/null subscription in KV\n", targetId)
			http.Error(w, "Target has no valid subscription (try re-subscribing)", http.StatusNotFound)
			return
		}

		if err := json.Unmarshal([]byte(subStr), s); err != nil {
			fmt.Fprintf(os.Stderr, "[Push] Error parsing subscription for %s: %v. Data: [%s]\n", targetId, err, subStr)
			http.Error(w, fmt.Sprintf("Invalid target subscription data: %v (Raw: %s)", err, subStr), http.StatusInternalServerError)
			return
		}

		// Prepare Payload from request body
		var pushPayload map[string]any
		json.NewDecoder(req.Body).Decode(&pushPayload)
		payloadBytes, _ := json.Marshal(pushPayload)

		fmt.Fprintf(os.Stderr, "[Push] Sending to endpoint: %s\n", s.Endpoint)
		resp, err := webpush.SendNotification(payloadBytes, s, &webpush.Options{
			HTTPClient:      http.DefaultClient,
			Subscriber:      subscriberEmail,
			VAPIDPublicKey:  vapidPublicKey,
			VAPIDPrivateKey: vapidPrivateKey,
			TTL:             30,
		})

		if err != nil {
			fmt.Fprintf(os.Stderr, "[Push] Error from SendNotification: %v\n", err)
			http.Error(w, fmt.Sprintf("Push delivery failed: %v", err), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		w.WriteHeader(resp.StatusCode)
		w.Write([]byte("Push sent"))
	})

	// Test push endpoint (no auth)
	mux.HandleFunc("/push/test/{peerId}", func(w http.ResponseWriter, req *http.Request) {
		targetId := req.PathValue("peerId")
		pingoKV, _ := kv.NewNamespace(kvNamespace)
		subStr, _ := pingoKV.GetString("push:"+targetId, nil)
		if subStr == "" {
			http.Error(w, "Target not registered", 404)
			return
		}

		var s webpush.Subscription
		json.Unmarshal([]byte(subStr), &s)

		payloadBytes, _ := json.Marshal(map[string]string{
			"title": "Prueba de Pingo",
			"body":  "Si ves esto, la entrega funciona a pesar de la seguridad.",
			"url":   "/",
		})

		resp, err := webpush.SendNotification(payloadBytes, &s, &webpush.Options{
			HTTPClient:      http.DefaultClient,
			Subscriber:      subscriberEmail,
			VAPIDPublicKey:  vapidPublicKey,
			VAPIDPrivateKey: vapidPrivateKey,
			TTL:             30,
		})

		if err != nil {
			fmt.Fprintf(os.Stderr, "[Push] Test Error: %v\n", err)
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "[Push] FCM Response (%d): %s\n", resp.StatusCode, string(respBody))

		w.WriteHeader(resp.StatusCode)
		w.Write([]byte("Push sent"))
	})

	// Git CORS Proxy
	mux.HandleFunc("/git-proxy/", func(w http.ResponseWriter, req *http.Request) {
		targetPath := strings.TrimPrefix(req.URL.Path, "/git-proxy/")
		if targetPath == "" {
			http.Error(w, "Target required", http.StatusBadRequest)
			return
		}

		// Determine target URL - assume https if not provided
		targetURL := targetPath
		if !strings.HasPrefix(targetURL, "http") {
			targetURL = "https://" + targetURL
		}

		if req.URL.RawQuery != "" {
			targetURL += "?" + req.URL.RawQuery
		}

		fmt.Fprintf(os.Stderr, "[GitProxy] Method: %s, Forwarding to: %s\n", req.Method, targetURL)

		proxyReq, err := http.NewRequest(req.Method, targetURL, req.Body)
		if err != nil {
			http.Error(w, "Request Creation Error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Copy relevant headers from client
		for name, values := range req.Header {
			normalizedName := strings.ToLower(name)
			// Skip headers that should be handled by the proxy or are internal
			if normalizedName == "host" || normalizedName == "x-pingo-auth" ||
				normalizedName == "cf-ray" || normalizedName == "cf-connecting-ip" ||
				normalizedName == "x-real-ip" || normalizedName == "x-forwarded-for" {
				continue
			}
			for _, value := range values {
				proxyReq.Header.Add(name, value)
			}
		}

		resp, err := http.DefaultClient.Do(proxyReq)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[GitProxy] Gateway Error: %v\n", err)
			http.Error(w, "Gateway Error: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Copy response headers back to client
		for name, values := range resp.Header {
			// Some headers shouldn't be copied back or will be overridden by CORS middleware
			normalizedName := strings.ToLower(name)
			if normalizedName == "access-control-allow-origin" || normalizedName == "access-control-allow-credentials" {
				continue
			}
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}

		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})

	// -------------------------------------------------------------
	// 4. DDNS Multitenant & Cloud Hub (klitosan.com)
	// -------------------------------------------------------------

	// /api/v1/ddns/register: Registra o autoriza un nuevo appliance (requiere admin token o secret)
	mux.HandleFunc("/api/v1/ddns/register", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		adminKey := cloudflare.Getenv("ADMIN_API_KEY")
		clientKey := req.Header.Get("X-Admin-Key")
		if adminKey != "" && clientKey != adminKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		var payload struct {
			ApplianceID string `json:"applianceId"`
			SecretToken string `json:"secretToken"`
			Subdomain   string `json:"subdomain"` // Opcional, ej: "casa.appliances.klitosan.com"
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil || payload.ApplianceID == "" || payload.SecretToken == "" {
			http.Error(w, "Invalid Payload. applianceId and secretToken required", http.StatusBadRequest)
			return
		}

		defaultDomain := cloudflare.Getenv("DEFAULT_DDNS_DOMAIN")
		if defaultDomain == "" {
			defaultDomain = "appliances.klitosan.com"
		}
		subdomain := payload.Subdomain
		if subdomain == "" {
			subdomain = fmt.Sprintf("%s.%s", strings.ToLower(payload.ApplianceID), defaultDomain)
		}

		tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(payload.SecretToken)))
		record := map[string]any{
			"applianceId":  payload.ApplianceID,
			"secretHash":   tokenHash,
			"subdomain":    subdomain,
			"lastIp":       "",
			"lastUpdate":   int64(0),
			"dnsRecordId":  "",
			"status":       "active",
		}

		pingoKV, err := kv.NewNamespace(kvNamespace)
		if err != nil {
			http.Error(w, "KV Error", http.StatusInternalServerError)
			return
		}
		data, _ := json.Marshal(record)
		pingoKV.PutString("appliance:"+payload.ApplianceID, string(data), nil)

		fmt.Fprintf(os.Stderr, "[DDNS] Appliance registrado: %s (%s)\n", payload.ApplianceID, subdomain)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":      "ok",
			"applianceId": payload.ApplianceID,
			"subdomain":   subdomain,
		})
	})

	// /api/v1/ddns/device-code: RFC 8628 Device Authorization Request
	mux.HandleFunc("/api/v1/ddns/device-code", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			ApplianceID string `json:"applianceId"`
			Subdomain   string `json:"subdomain"`
		}
		json.NewDecoder(req.Body).Decode(&payload)
		if payload.ApplianceID == "" {
			http.Error(w, "applianceId is required", http.StatusBadRequest)
			return
		}

		deviceCode := generateAuthToken(fmt.Sprintf("%s-%d", payload.ApplianceID, time.Now().UnixNano()))
		// Generar un userCode legible (ej: ABCD-1234)
		userCodeSeed := fmt.Sprintf("%x", sha256.Sum256([]byte(deviceCode)))
		userCode := fmt.Sprintf("%s-%s", strings.ToUpper(userCodeSeed[:4]), strings.ToUpper(userCodeSeed[4:8]))

		pingoKV, err := kv.NewNamespace(kvNamespace)
		if err != nil {
			http.Error(w, "KV Error", http.StatusInternalServerError)
			return
		}

		reqHost := req.Host
		if reqHost == "" {
			reqHost = "pingo-cloud.accreativos.com"
		}
		proto := "https"
		verificationURI := fmt.Sprintf("%s://%s/ddns/activate", proto, reqHost)
		verificationURIComplete := fmt.Sprintf("%s?code=%s", verificationURI, userCode)

		deviceSession := map[string]any{
			"deviceCode":   deviceCode,
			"userCode":     userCode,
			"applianceId":  payload.ApplianceID,
			"subdomain":    payload.Subdomain,
			"status":       "authorization_pending",
			"secretToken":  "",
			"expiresAt":    time.Now().Unix() + 600, // 10 minutos
		}

		data, _ := json.Marshal(deviceSession)
		pingoKV.PutString("devcode:"+deviceCode, string(data), nil)
		pingoKV.PutString("usercode:"+userCode, deviceCode, nil)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"device_code":               deviceCode,
			"user_code":                 userCode,
			"verification_uri":          verificationURI,
			"verification_uri_complete": verificationURIComplete,
			"expires_in":                600,
			"interval":                  2,
		})
	})

	// /api/v1/ddns/device-token: Polling del CLI para recoger el token una vez aprobado
	mux.HandleFunc("/api/v1/ddns/device-token", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			DeviceCode string `json:"device_code"`
		}
		json.NewDecoder(req.Body).Decode(&payload)
		if payload.DeviceCode == "" {
			http.Error(w, "device_code is required", http.StatusBadRequest)
			return
		}

		pingoKV, err := kv.NewNamespace(kvNamespace)
		if err != nil {
			http.Error(w, "KV Error", http.StatusInternalServerError)
			return
		}

		sessionStr, err := pingoKV.GetString("devcode:"+payload.DeviceCode, nil)
		if err != nil || sessionStr == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "Device code not found or expired"})
			return
		}

		var session struct {
			DeviceCode  string `json:"deviceCode"`
			UserCode    string `json:"userCode"`
			ApplianceID string `json:"applianceId"`
			Subdomain   string `json:"subdomain"`
			Status      string `json:"status"`
			SecretToken string `json:"secretToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		}
		json.Unmarshal([]byte(sessionStr), &session)

		if time.Now().Unix() > session.ExpiresAt {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "expired_token", "error_description": "The device authorization has expired"})
			return
		}

		if session.Status == "authorization_pending" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending", "error_description": "Waiting for user authorization"})
			return
		}

		if session.Status != "approved" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"error": "access_denied", "error_description": "Device authorization denied"})
			return
		}

		// Aprobado con éxito: devolver credenciales completas
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"applianceId":  session.ApplianceID,
			"subdomain":    session.Subdomain,
			"secret_token": session.SecretToken,
			"token_type":   "Bearer",
		})
	})

	// /ddns/activate: Página web interactiva responsive para que el usuario introduzca o confirme el código
	mux.HandleFunc("/ddns/activate", func(w http.ResponseWriter, req *http.Request) {
		code := req.URL.Query().Get("code")
		html := fmt.Sprintf(`<!DOCTYPE html>
<html lang="es">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Autorizar Appliance — Cloud Hub</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #0f172a; color: #f8fafc; display: flex; align-items: center; justify-content: center; min-height: 100vh; margin: 0; padding: 20px; box-sizing: border-box; }
    .card { background: #1e293b; border: 1px solid #334155; border-radius: 16px; padding: 32px; max-width: 440px; width: 100%; box-shadow: 0 20px 25px -5px rgba(0,0,0,0.5); }
    h1 { font-size: 1.5rem; margin-top: 0; display: flex; align-items: center; gap: 10px; color: #38bdf8; }
    p { color: #94a3b8; font-size: 0.95rem; line-height: 1.5; }
    .input-group { margin: 24px 0; }
    label { display: block; font-size: 0.85rem; font-weight: 600; text-transform: uppercase; letter-spacing: 0.05em; color: #cbd5e1; margin-bottom: 8px; }
    input { width: 100%; padding: 14px; font-size: 1.25rem; font-weight: bold; text-align: center; letter-spacing: 0.2em; background: #0f172a; border: 2px solid #38bdf8; border-radius: 8px; color: #fff; box-sizing: border-box; outline: none; }
    button { width: 100%; padding: 14px; background: #0284c7; color: white; border: none; border-radius: 8px; font-size: 1rem; font-weight: 600; cursor: pointer; transition: background 0.2s; }
    button:hover { background: #0369a1; }
    #msg { margin-top: 16px; padding: 12px; border-radius: 8px; display: none; font-size: 0.95rem; text-align: center; }
    .success { background: #064e3b; color: #6ee7b7; border: 1px solid #059669; }
    .error { background: #7f1d1d; color: #fca5a5; border: 1px solid #dc2626; }
  </style>
</head>
<body>
  <div class="card">
    <h1>🛡️ Cloud Hub DDNS</h1>
    <p>Introduce o confirma el código de autorización mostrado en la terminal de tu appliance para vincularlo a tu dominio.</p>
    <div class="input-group">
      <label for="code">Código de Dispositivo</label>
      <input id="code" value="%s" placeholder="ABCD-1234" maxlength="9" autofocus autocomplete="off" autocorrect="off">
    </div>
    <button id="btn-auth" onclick="approve()">Autorizar Dispositivo</button>
    <div id="msg"></div>
  </div>
  <script>
    async function approve() {
      const code = document.getElementById('code').value.trim();
      const msg = document.getElementById('msg');
      const btn = document.getElementById('btn-auth');
      if (!code) return;
      btn.disabled = true;
      btn.innerText = 'Autorizando...';
      try {
        const res = await fetch('/api/v1/ddns/device-approve', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ user_code: code })
        });
        const data = await res.json();
        if (res.ok) {
          msg.className = 'success';
          msg.innerHTML = '✅ ¡Dispositivo autorizado!<br><small>Puedes volver a tu terminal. El appliance se ha configurado automáticamente.</small>';
          msg.style.display = 'block';
          btn.style.display = 'none';
        } else {
          msg.className = 'error';
          msg.innerText = '❌ Error: ' + (data.error_description || data.error || 'Código no válido');
          msg.style.display = 'block';
          btn.disabled = false;
          btn.innerText = 'Reintentar';
        }
      } catch (e) {
        msg.className = 'error';
        msg.innerText = '❌ Error de red: ' + e.message;
        msg.style.display = 'block';
        btn.disabled = false;
        btn.innerText = 'Reintentar';
      }
    }
  </script>
</body>
</html>`, code)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(html))
	})

	// /api/v1/ddns/device-approve: Acción al pulsar "Autorizar" en la web
	mux.HandleFunc("/api/v1/ddns/device-approve", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			UserCode string `json:"user_code"`
		}
		json.NewDecoder(req.Body).Decode(&payload)
		userCode := strings.ToUpper(strings.TrimSpace(payload.UserCode))

		pingoKV, err := kv.NewNamespace(kvNamespace)
		if err != nil {
			http.Error(w, "KV Error", http.StatusInternalServerError)
			return
		}

		deviceCode, err := pingoKV.GetString("usercode:"+userCode, nil)
		if err != nil || deviceCode == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not_found", "error_description": "Código de autorización no encontrado o caducado"})
			return
		}

		sessionStr, err := pingoKV.GetString("devcode:"+deviceCode, nil)
		if err != nil || sessionStr == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not_found", "error_description": "Sesión de dispositivo expirada"})
			return
		}

		var session struct {
			DeviceCode  string `json:"deviceCode"`
			UserCode    string `json:"userCode"`
			ApplianceID string `json:"applianceId"`
			Subdomain   string `json:"subdomain"`
			Status      string `json:"status"`
			SecretToken string `json:"secretToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		}
		json.Unmarshal([]byte(sessionStr), &session)

		// Generar token secreto de larga duración para el appliance
		generatedSecretToken := generateAuthToken(fmt.Sprintf("secret-%s-%d", session.ApplianceID, time.Now().UnixNano()))

		defaultDomain := cloudflare.Getenv("DEFAULT_DDNS_DOMAIN")
		if defaultDomain == "" {
			defaultDomain = "appliances.klitosan.com"
		}
		subdomain := session.Subdomain
		if subdomain == "" {
			subdomain = fmt.Sprintf("%s.%s", strings.ToLower(session.ApplianceID), defaultDomain)
		}

		tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(generatedSecretToken)))
		applianceRecord := map[string]any{
			"applianceId": session.ApplianceID,
			"secretHash":  tokenHash,
			"subdomain":   subdomain,
			"lastIp":      "",
			"lastUpdate":  int64(0),
			"dnsRecordId": "",
			"status":      "active",
		}

		appData, _ := json.Marshal(applianceRecord)
		pingoKV.PutString("appliance:"+session.ApplianceID, string(appData), nil)

		// Actualizar sesión del dispositivo a approved
		session.Status = "approved"
		session.SecretToken = generatedSecretToken
		session.Subdomain = subdomain
		sessData, _ := json.Marshal(session)
		pingoKV.PutString("devcode:"+deviceCode, string(sessData), nil)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":      "ok",
			"applianceId": session.ApplianceID,
			"subdomain":   subdomain,
		})
	})

	// /api/v1/ddns/heartbeat: Latido del appliance para actualizar su IP pública dinámicamente con cuota protegida
	mux.HandleFunc("/api/v1/ddns/heartbeat", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost && req.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		applianceID := req.Header.Get("X-Appliance-ID")
		authToken := req.Header.Get("Authorization")
		if applianceID == "" {
			applianceID = req.URL.Query().Get("id")
		}
		if authToken == "" {
			authToken = req.URL.Query().Get("token")
		} else {
			authToken = strings.TrimPrefix(authToken, "Bearer ")
		}

		if applianceID == "" || authToken == "" {
			http.Error(w, "Missing appliance credentials (id and token)", http.StatusUnauthorized)
			return
		}

		pingoKV, err := kv.NewNamespace(kvNamespace)
		if err != nil {
			http.Error(w, "KV Error", http.StatusInternalServerError)
			return
		}

		recordStr, err := pingoKV.GetString("appliance:"+applianceID, nil)
		if err != nil || recordStr == "" {
			http.Error(w, "Appliance not found or unauthorized", http.StatusUnauthorized)
			return
		}

		var record struct {
			ApplianceID string `json:"applianceId"`
			SecretHash  string `json:"secretHash"`
			Subdomain   string `json:"subdomain"`
			LastIP      string `json:"lastIp"`
			LastUpdate  int64  `json:"lastUpdate"`
			DNSRecordID string `json:"dnsRecordId"`
			Status      string `json:"status"`
		}
		if err := json.Unmarshal([]byte(recordStr), &record); err != nil || record.Status != "active" {
			http.Error(w, "Invalid appliance state or inactive", http.StatusForbidden)
			return
		}

		// Validar token secreto
		tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(authToken)))
		if tokenHash != record.SecretHash {
			http.Error(w, "Invalid authentication token", http.StatusUnauthorized)
			return
		}

		// 1. Obtener IP pública real del cliente a través de Cloudflare Edge
		clientIP := req.Header.Get("CF-Connecting-IP")
		if clientIP == "" {
			clientIP = req.Header.Get("X-Real-IP")
		}
		if clientIP == "" {
			clientIP = strings.Split(req.RemoteAddr, ":")[0]
		}

		now := time.Now().Unix()

		// 2. PROTECCIÓN DE CUOTAS (Zero-API Cost):
		// Si la IP no ha cambiado, no llamamos a la API de DNS de Cloudflare.
		if clientIP == record.LastIP && record.DNSRecordID != "" {
			// Throttle KV updates a no más de 1 vez cada 5 minutos
			if now-record.LastUpdate > 300 {
				record.LastUpdate = now
				data, _ := json.Marshal(record)
				pingoKV.PutString("appliance:"+applianceID, string(data), nil)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"status":    "unchanged",
				"ip":        clientIP,
				"subdomain": record.Subdomain,
				"echReady":  true,
			})
			return
		}

		// 3. LA IP HA CAMBIADO O ES EL PRIMER REGISTRO: Invocar API de Cloudflare DNS
		cfAPIToken := cloudflare.Getenv("CLOUDFLARE_API_TOKEN")
		zoneID := cloudflare.Getenv("CLOUDFLARE_ZONE_ID")

		dnsAction := "updated"
		newDNSRecordID := record.DNSRecordID

		if cfAPIToken != "" && zoneID != "" {
			dnsPayload := map[string]any{
				"type":    "A",
				"name":    record.Subdomain,
				"content": clientIP,
				"ttl":     1,    // Auto
				"proxied": false, // Tráfico directo a appliance (o true si pasa por CDN)
			}
			payloadBytes, _ := json.Marshal(dnsPayload)

			var dnsURL, httpMethod string
			if record.DNSRecordID == "" {
				dnsURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", zoneID)
				httpMethod = http.MethodPost
			} else {
				dnsURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zoneID, record.DNSRecordID)
				httpMethod = http.MethodPut
			}

			cfReq, _ := http.NewRequest(httpMethod, dnsURL, strings.NewReader(string(payloadBytes)))
			cfReq.Header.Set("Authorization", "Bearer "+cfAPIToken)
			cfReq.Header.Set("Content-Type", "application/json")

			cfResp, cfErr := http.DefaultClient.Do(cfReq)
			if cfErr != nil {
				fmt.Fprintf(os.Stderr, "[DDNS] Error calling Cloudflare API: %v\n", cfErr)
			} else {
				defer cfResp.Body.Close()
				var cfResult struct {
					Success bool `json:"success"`
					Result  struct {
						ID string `json:"id"`
					} `json:"result"`
				}
				json.NewDecoder(cfResp.Body).Decode(&cfResult)
				if cfResult.Success && cfResult.Result.ID != "" {
					newDNSRecordID = cfResult.Result.ID
				}
			}
		} else {
			dnsAction = "simulated_no_cf_credentials"
		}

		// 4. Persistir estado en KV
		record.LastIP = clientIP
		record.LastUpdate = now
		record.DNSRecordID = newDNSRecordID
		data, _ := json.Marshal(record)
		pingoKV.PutString("appliance:"+applianceID, string(data), nil)

		fmt.Fprintf(os.Stderr, "[DDNS] IP actualizada para %s -> %s (Subdominio: %s)\n", applianceID, clientIP, record.Subdomain)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":    dnsAction,
			"ip":        clientIP,
			"subdomain": record.Subdomain,
			"echReady":  true,
		})
	})

	// Setup CORS and Middleware
	c := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders: []string{"Content-Type", "X-Pingo-Auth", "Authorization", "X-Admin-Key", "X-Appliance-ID"},
	})
	handler := recoverMiddleware(c.Handler(mux))

	workers.Serve(handler)
}
