# anyIP proxy smoke test

Web Harvester already supports authenticated HTTP proxy URLs through its generic proxy pool. The optional `HARVESTER_PROXY_URL` environment variable overrides the YAML `proxies` list with one proxy URL, allowing credentials to stay out of tracked configuration.

## 1. Set credentials locally

Use the replacement credentials from your anyIP dashboard. Rotate any credentials that have been pasted into chats, tickets, logs, or source control.

```bash
export ANYIP_PROXY_USER='your_anyip_username'
export ANYIP_PROXY_PASSWORD='your_anyip_password'
export HARVESTER_PROXY_URL="http://${ANYIP_PROXY_USER}:${ANYIP_PROXY_PASSWORD}@portal.anyip.io:1080"
```

If the password contains URL-reserved characters, URL-encode the username and password before composing the proxy URL. Do not put real credentials in this document, YAML files, screenshots, command output, or commits. The `.env` pattern is ignored by Git in this repository, but still check `git status` before committing.

## 2. Verify the proxy separately

```bash
curl --fail --silent --show-error \
  --proxy "$HARVESTER_PROXY_URL" \
  --connect-timeout 10 --max-time 20 \
  https://httpbin.org/ip
```

A successful response returns the public IP observed by httpbin. This is a connectivity smoke test, not a state-level geolocation test or a guarantee that a target website will return usable content.

## 3. Use it in Web Harvester

Set `proxies: []` in your local YAML config, or leave existing proxies configured; a non-empty `HARVESTER_PROXY_URL` takes precedence and configures exactly one proxy. The application will use it through the existing proxy pool, including its health tracking and cooldown behavior.

For a low-volume test, choose an explicitly authorized target and conservative settings. The default config mode is `mock`, so setting the proxy alone does not cause network traffic. Live network modes require the repository's existing explicit terms-of-service acknowledgement flag. Do not use this setup to bypass authentication, paywalls, CAPTCHAs, or access controls.

## 4. What this does not do

- It does not add a vendor-specific anyIP SDK or assume anyIP API behavior beyond the supplied HTTP proxy endpoint.
- It does not enable sticky sessions or state targeting by itself. Those options must be configured in the provider dashboard or according to anyIP's current documentation.
- It does not prove a request was billed at a particular amount. Reconcile traffic with the anyIP dashboard after a small test.
