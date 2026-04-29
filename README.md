# ☁️ Pingo Backend & Infrastructure

Este directorio contiene el código y la configuración para los servicios opcionales de Pingo (Relé TURN y Notificaciones Push).

## 📂 Estructura
- `/backend`: Cloudflare Worker (Go/TinyGo) para gestión de credenciales y push.
- `/coturn`: Configuración de Docker para el servidor TURN/STUN.

## 🚀 Despliegue del Servidor TURN (Relé)
El servidor TURN permite la conexión cuando el P2P directo falla en redes 4G/5G.

1. Instala Docker y Docker Compose en tu servidor.
2. Navega a `coturn/`.
3. Edita `coturn.conf` si necesitas cambiar puertos (por defecto 3478).
4. Levanta el servicio:
   ```bash
   docker-compose up -d
   ```

## ⚡ Despliegue del Cloudflare Worker (API)
El backend gestiona la seguridad y las claves temporales para Coturn.

1. Instala Wrangler: `npm install -g wrangler`.
2. Navega a `backend/`.
3. Crea el KV Namespace en Cloudflare:
   ```bash
   wrangler kv:namespace create PINGO_AUTH
   ```
4. Actualiza `wrangler.jsonc` con el `id` obtenido y tu `TURN_URL`.
5. Despliega:
   ```bash
   wrangler deploy
   ```

## 🔒 Seguridad (Sincronización)
El sistema utiliza un mecanismo de **Shared Secret**:
- El `static-auth-secret` en `coturn.conf` DEBE coincidir con el `TURN_STATIC_AUTH_SECRET` en `wrangler.jsonc`.
- Los usuarios se autorizan mediante un Token basado en su `salt` privado, por lo que solo los que tú registres podrán usar tu relé.
# backend
