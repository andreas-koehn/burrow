---
layout: home
hero:
  name: Burrow
  text: Self-hosted reverse tunnels — one container, automatic HTTPS, your domain
  tagline: Run a relay on your own server. Connect any local port. Get a public URL under your domain with TLS handled automatically — no Nginx, no Certbot, no third-party service.
  actions:
    - theme: brand
      text: Deploy in 5 minutes
      link: /guide/quickstart
    - theme: alt
      text: Introduction
      link: /guide/introduction
    - theme: alt
      text: GitHub
      link: https://github.com/ankoehn/burrow
features:
  - title: Self-hosted — no SaaS, no accounts
    details: The relay runs on your server. No third party sees your traffic. No usage quotas you didn't configure. No account required to connect a client.
  - title: Automatic HTTPS — built-in Let's Encrypt
    details: Set BURROW_ACME_DOMAIN and BURROW_ACME_EMAIL. burrowd obtains and renews certificates via ACME before the first listener starts. One container, ports 80 and 443, done.
  - title: HTTP tunnels and TCP tunnels with access control
    details: HTTP services are served at a path on your relay's domain (https://burrow.insingo.com/svc/<slug>/), so one A record and one certificate are enough. TCP ports are forwarded directly. Access modes — open, api_key, burrow_login, or mTLS — are set per service.
  - title: Apache-2.0 — no open core
    details: Every feature is in the repository. Nothing is held back for a paid tier. Build from source or pull the pre-built image.
---

## Get a tunnel running

A minimal Docker Compose stack — ACME TLS included:

```yaml
# compose.yml
services:
  burrowd:
    image: ghcr.io/ankoehn/burrow:develop
    command: burrowd serve
    ports:
      - "80:80"
      - "443:443"
      - "7000:7000"
    volumes:
      - burrow_data:/data
    environment:
      BURROW_DATABASE_PATH: /data/burrow.db
      BURROW_ACME_DOMAIN: burrow.insingo.com
      BURROW_ACME_EMAIL: admin@insingo.com
      BURROW_ADMIN_EMAIL: admin@insingo.com
      BURROW_ADMIN_PASSWORD: changeme
      BURROW_HTTP_SECURE_COOKIES: "true"
volumes:
  burrow_data:
```

Then connect a local service from any machine:

```sh
curl -fsSL https://burrow.insingo.com/install.sh | sh
burrow login burrow.insingo.com
burrow http 3000
```

Your app is live at `https://burrow.insingo.com/svc/k7p2qx/`. The client prints the exact URL on `tunnel registered`.

::: tip No separate auth domain needed
When `BURROW_ACME_DOMAIN` is set, burrowd infers the base domain from it automatically — you do not need `BURROW_AUTH_DOMAIN`.
:::

::: info No stable release yet
Burrow does not have a tagged release. Use the `develop` channel: image `ghcr.io/ankoehn/burrow:develop` or [pre-built binaries](https://github.com/andreas-koehn/burrow/releases/tag/develop). The GHCR package is private by default — make it public or `docker login ghcr.io` before pulling.
:::

See the [Quickstart](/guide/quickstart) for the full step-by-step walkthrough, or [Deploy on a server](/guide/deploy) for a production setup with firewall rules and first-boot verification.
