<div align="center">

# 🛰️ Relay

### Your apps, online with HTTPS in a minute. No config files, no stress.

A friendly, self-hosted **reverse proxy & load balancer manager** with a beautiful web UI.
Every change is checked ✅, versioned 🗂️ and **rolled back automatically** if something breaks ↩️.

[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](LICENSE)
![Self-hosted](https://img.shields.io/badge/self--hosted-🏠-success)
![Docker](https://img.shields.io/badge/runs%20on-Docker-2496ED)

[🚀 Quick start](#-quick-start) · [✨ Features](#-features) · [⚡ Relay Edge](#-relay-edge-the-recommended-proxy-engine) · [🆘 Help](#-troubleshooting) · [📚 Docs](#-documentation)

</div>

---

## 🤔 Why Relay?

Most proxy tools apply your edit the second you click save. One typo… and every site is down. 😬

Relay does it the calm way:

| Step | What happens |
|---|---|
| ✏️ **Edit** | Change hosts, certificates or rules. Nothing goes live yet. |
| 👀 **Review** | See exactly what will change in a clear diff. |
| 🚀 **Apply** | Relay checks the config, reloads with zero downtime and tests your sites. |
| ↩️ **Oops?** | If a site that worked before breaks, Relay puts the old config back **automatically**. |

Every apply is saved in **Config history**, so you can always see who changed what, and roll back with one click. 🕰️

---

## ✨ Features

<table>
<tr>
<td valign="top" width="50%">

### 🌐 Reverse proxy
- 🔒 Automatic HTTPS with Let's Encrypt (wildcards too)
- 🧭 Per-path rules, redirects, websockets, headers
- ⚡ HTTP/2 and HTTP/3
- 🎮 TCP/UDP streams (game servers, databases, MQTT…)
- 🛰️ Pick your engine: **Relay Edge** or **nginx**

### 🛡️ Protection
- 🚪 **Relay login**: a built-in login page for any app, with 2FA
- 🔑 Single sign-on with Authelia, Authentik or oauth2-proxy
- 📋 Access lists (IP rules and passwords)
- 🚦 Rate limiting, exploit blocking, IP blocklist

</td>
<td valign="top" width="50%">

### ⚖️ Load balancing
- ❤️ HAProxy with health checks and sticky sessions
- 🔄 Drain servers for zero-downtime deploys
- 🪄 *Expose* wizard: backend → public domain in one go

### 🤖 Automation
- 🐳 Docker discovery (local and remote hosts)
- 🌍 Public DNS: records created for you at GoDaddy or Cloudflare
- 📦 Import from Nginx Proxy Manager
- 🧠 Manage Relay from Claude and other AI assistants (MCP)

### 📊 Day-to-day
- 📈 Dashboard, access, error and audit logs
- 🚧 Maintenance mode and custom error pages
- 🔔 Notifications, 💾 encrypted backups, 👥 users with 2FA
- ⬆️ One-click upgrades, 📚 built-in docs

</td>
</tr>
</table>

---

## ⚡ Relay Edge: the recommended proxy engine

Relay can run your traffic through two engines. You choose in **Settings → Proxy engine**, and you can switch back and forth anytime: your hosts, certificates and rules are shared, and every switch is health-checked and rolled back if anything fails.

> 🧪 **Relay Edge is our recommended engine, but it's still in beta.**
> It's fast and packed with improvements, but nginx has decades of battle-testing behind it.
> **nginx stays the default** for now, so choose Relay Edge when you're ready to try it. Switching back takes one click. 🙂

### Why Relay Edge? 🏆

| | ⚡ Relay Edge | 🟩 nginx |
|---|---|---|
| **Reloads** | Config and certificate changes **never drop a connection** | A reload is quick, but long-running connections can be cut |
| **Bad config?** | A failed reload keeps serving the old config | Stops at validation (Relay checks it first) |
| **Renewed certificates** | Picked up automatically | Needs a reload |
| **Speed to your apps** | Keeps connections to your apps open and reuses them | New connection per request by default |
| **Monitoring** | Built-in Prometheus `/metrics` and health endpoint | Basic connection counters only |
| **Security** | Written in memory-safe Go, modern TLS with post-quantum key exchange, strips spoofed login headers | Proven C codebase with OpenSSL |
| **Updates** | Built into Relay: updates together with Relay | Separate container image to keep up to date |
| **Custom nginx snippets** | ❌ Not run (kept, and used again if you switch back) | ✅ Supported |
| **Maturity** | 🧪 Beta | 🏔️ Rock solid |

**In short:** 🟢 pick **Relay Edge** for smoother reloads, better performance to your apps and built-in metrics. 🟩 Stick with **nginx** if you rely on custom nginx snippets or want the most battle-tested option.

<details>
<summary>📝 Good to know about Relay Edge</summary>

- Geo-blocking by country isn't enforced by either engine yet.
- PROXY protocol on **UDP** streams isn't sent (TCP streams work).
- Only one engine runs at a time; Relay stops the other one and its container.

</details>

---

## 🚀 Quick start

### 🧰 What you need

- 🐧 A **Linux server** (a VPS, a home server or a VM)
- 🐳 **Docker** with the **Compose plugin** ([install Docker](https://docs.docker.com/engine/install/))
- 🌱 **git**
- 🚪 Ports **80** and **443** free (no other web server or proxy using them)

> 💡 You don't need Go or Node.js, because everything builds inside Docker.
> 🍎 Docker Desktop on macOS/Windows is fine for a test drive, but use Linux for real traffic (Relay uses host networking).

### 1️⃣ Download Relay

```bash
git clone https://github.com/ollieInstantOffr/app-relay.git relay
cd relay
```

### 2️⃣ Start it

```bash
make up
```

<details>
<summary>No <code>make</code>? Use this instead 👇</summary>

```bash
RELAY_VERSION=$(sh scripts/version.sh) RELAY_COMMIT=$(git rev-parse HEAD) docker compose up -d --build
```

</details>

☕ The first build takes a few minutes. Check everything is up with `docker compose ps`.

### 3️⃣ Say hello 👋

Open **`http://<your-server-ip>:8181`** and the setup wizard helps you:

1. 👤 create your admin account
2. 🔍 check your network and ports
3. 🌐 (optional) put Relay itself on a domain like `relay.example.com`

🎉 **That's it, Relay is running!**

---

## 🌍 Your first site in 5 steps

Say your app runs on `192.168.1.20:3000` and you want it at `https://app.example.com`:

1. 🧭 **Point your domain** at the server: an `A` record for `app.example.com` → your server's public IP.
   *(Relay's Public DNS integration can do this for you.)*
2. ➕ In Relay, open **Proxy hosts** and click **New host**.
3. ✏️ Enter `app.example.com` and forward it to `http://192.168.1.20:3000`.
4. 🔒 On the **SSL** tab, click **Request new** for a free certificate and turn on **Force HTTPS**.
5. 🚀 **Save**, then hit **Apply & reload** in the bar at the top.

Open `https://app.example.com`. **Done!** 🥳

> 🐳 Running apps in Docker? Go to **Settings → Docker discovery** and Relay suggests hosts for your containers.

---

## ⬆️ Updating

✨ **The easy way:** **Settings → Updates → Upgrade**. Relay pulls the latest version, rebuilds, restarts, and rolls back if something goes wrong. Your sites stay online.

⌨️ **The terminal way:**

```bash
cd relay && git pull && make up
```

> ⚠️ Run Relay from a folder you don't edit by hand, because upgrades build from it.

---

## 🧑‍💻 Handy commands

| 🎯 What | ⌨️ Command |
|---|---|
| Start or rebuild | `make up` |
| Stop | `make down` |
| Watch the logs | `make logs` |
| Container status | `docker compose ps` |
| Reset a password | `docker exec -it relay relay users reset-password <username>` |

---

## 🔥 Ports and firewall

| Port | What for | Open to the internet? |
|---|---|---|
| `80/tcp` | HTTP and certificate checks | ✅ Yes |
| `443/tcp` | HTTPS | ✅ Yes |
| `443/udp` | HTTP/3 (if you turn it on) | 🤷 Optional |
| `8181/tcp` | Relay's admin UI | 🚫 **No**, keep it on your LAN or VPN |
| your stream ports | TCP/UDP streams you create | Only the ones you need |

🔐 **Tip:** put the admin UI on its own domain with HTTPS (**Settings → General → Admin UI domain**) and limit who can reach it (**Settings → Users & access**).

---

## 💾 Backups

Everything important lives in the `relay-data` Docker volume.
Use **Settings → Backup & restore** for encrypted backups, on a schedule or whenever you like. And every change you apply is kept in **Config history** too. 🕰️

---

## 🆘 Troubleshooting

<details>
<summary>😕 I can't open <code>http://&lt;server-ip&gt;:8181</code></summary>

Check the containers run (`docker compose ps`) and look at `make logs`. Make sure your firewall or cloud security group allows port 8181 from your network.
</details>

<details>
<summary>🔒 My certificate request fails</summary>

The domain must point at this server and port **80** must be reachable from the internet. Behind a home router, forward ports 80 and 443 to the server. Or use a DNS provider (DNS-01), which also works for `*.example.com`.
</details>

<details>
<summary>🚪 "Address already in use" on port 80 or 443</summary>

Another web server is running. Stop it (for example `sudo systemctl stop nginx`) or change Relay's ports in **Settings → General**.
</details>

<details>
<summary>💥 My site shows 502 Bad Gateway</summary>

Relay can't reach your app. Make sure it's running and that the address works **from the server** (`curl http://192.168.1.20:3000`). For apps in other containers, use the server's LAN IP, not `localhost`.
</details>

<details>
<summary>🔑 I'm locked out</summary>

```bash
docker exec -it relay relay users reset-password <username>
```
</details>

More answers live in the built-in docs under **Troubleshooting**. 📖

---

## 📚 Documentation

- 📖 **Built-in docs:** click **Docs** in Relay's menu for friendly step-by-step guides with examples.
- 🏗️ [deploy/README.md](deploy/README.md): containers, ports, volumes, how applying works
- ⬆️ [deploy/UPGRADES.md](deploy/UPGRADES.md): upgrading Relay, nginx and HAProxy
- ⚡ [docs/EDGE.md](docs/EDGE.md): Relay Edge under the hood

### 🤖 Use Relay from your AI assistant

Turn on **Settings → MCP server**, create a token, and connect. For example, with Claude Code:

```bash
claude mcp add --transport http relay https://relay.example.com/mcp \
  --header "Authorization: Bearer rl_mcp_…"
```

Then just ask: *"Put grafana.example.com behind a login page and apply."* ✨
Changes wait for your 👍 in **Logs → Approvals**.

### 🧩 How it's built

| Container | Job |
|---|---|
| `relay` | 🖥️ Web UI, API, AI server, database, certificates, Docker discovery |
| `relay-edge` | ⚡ Relay Edge, when it's your proxy engine |
| `relay-nginx` | 🟩 nginx, when it's your proxy engine |
| `relay-haproxy` | ⚖️ HAProxy, once you add a load balancer backend |

Only what you use is running; Relay stops the rest. 🌱

---

## 🤝 Contributing, 🔐 security & 📜 license

- 🤝 **Want to help?** Awesome! See [CONTRIBUTING.md](CONTRIBUTING.md).
- 🔐 **Found a security issue?** Please report it privately: [SECURITY.md](SECURITY.md).
- 📜 **License:** Relay is free software under the [GNU AGPL v3.0](LICENSE). Use it, self-host it, change it and share it for free, even commercially. If you share a modified version, or run one as a service for others, publish your source code under the same license. 💚

<div align="center">

Made with ☕ and 💚 · Copyright (C) 2026 InstantOffr

**If Relay makes your life easier, give it a ⭐!**

</div>
