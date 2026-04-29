# ☁️ P2PT Backend & Infrastructure

This directory contains the code and configuration for optional P2PT services (TURN Relay and Push Notifications).

## 📂 Structure
- `/backend`: Cloudflare Worker (Go/TinyGo) for credential management and push notifications.
- `/coturn`: Docker configuration for the TURN/STUN server.

## 🚀 TURN Server Deployment (Relay)
The TURN server enables connections when direct P2P fails on 4G/5G networks.

1. Install Docker and Docker Compose on your server.
2. Navigate to `coturn/`.
3. Edit `coturn.conf` if you need to change ports (default is 3478).
4. Start the service:
   ```bash
   docker-compose up -d
   ```

## ⚡ Cloudflare Worker Deployment (API)
The backend manages security and temporary keys for Coturn.

1. Install Wrangler: `npm install -g wrangler`.
2. Navigate to `backend/`.
3. Create the KV Namespace in Cloudflare:
   ```bash
   wrangler kv:namespace create P2PT
   ```
4. Update `wrangler.jsonc` with the obtained `id`.
5. Set secrets (VAPID and TURN):
   ```bash
   wrangler secret put VAPID_PRIVATE_KEY
   wrangler secret put VAPID_PUBLIC_KEY
   wrangler secret put TURN_URL
   wrangler secret put TURN_USERNAME
   wrangler secret put TURN_CREDENTIAL
   wrangler secret put TURN_STATIC_AUTH_SECRET
   ```
6. Deploy:
   ```bash
   wrangler deploy
   ```

## 🔒 Security (Synchronization)
The system uses a **Shared Secret** mechanism:
- The `static-auth-secret` in `coturn.conf` MUST match the `TURN_STATIC_AUTH_SECRET` in `wrangler.jsonc`.
- Users are authorized via a Token based on their private `salt`, so only those you register will be able to use your relay.
