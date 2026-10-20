# OAuth for IMAP and POP

Mailvault supports Microsoft OAuth device authorization and Google's desktop
OAuth authorization-code flow. Both use public OAuth clients and store refresh
tokens in the operating-system keyring by default. OAuth requires TLS on the
mail source and does not use `password_ref`.

## Microsoft

Register a public client in Microsoft Entra, enable public client flows, and
grant the delegated `IMAP.AccessAsUser.All` and `POP.AccessAsUser.All`
permissions. Mailvault requests the matching Outlook protocol scopes plus
`offline_access` so it can refresh access tokens without repeating device
login. Supply the app's client ID; Mailvault does not ship an application ID
or require a client secret.

```yaml
sources:
  - name: outlook
    type: imap
    host: outlook.office365.com
    port: 993
    username: user@outlook.com
    use_tls: true
    oauth2:
      provider: microsoft # optional; this is the default
      client_id: YOUR_MICROSOFT_PUBLIC_CLIENT_ID
      tenant: consumers
```

## Google

Create an OAuth client with application type **Desktop app** in Google Cloud,
enable the Gmail API, and configure the OAuth consent screen. Mailvault uses
the desktop loopback flow with a random local port, a one-time state value, and
PKCE. It requests the `https://mail.google.com/` scope for IMAP/POP access and
offline access for token refresh. The authorization URL is printed for you to
open in a browser; the callback is accepted only on the local loopback
listener.

```yaml
sources:
  - name: gmail
    type: imap
    host: imap.gmail.com
    port: 993
    username: user@gmail.com
    use_tls: true
    oauth2:
      provider: google
      client_id: YOUR_GOOGLE_DESKTOP_CLIENT_ID
```

Google desktop apps support loopback IP redirects and PKCE. See [OAuth 2.0 for
Desktop Apps](https://developers.google.com/identity/protocols/oauth2/native-app)
and the [Gmail API authorization scopes](https://developers.google.com/gmail/api/auth/scopes).

Authorize either source once with:

```sh
mailvault --config ./config.yaml auth login --source SOURCE_NAME
```

Microsoft displays a short verification code. Google prints an authorization
URL; after consent, its redirect reaches the local callback and completes the
login. Device codes, authorization codes, access tokens, and refresh tokens are
never printed or saved in the YAML configuration.

The OS keyring stores the token set under service `mailvault`. By default, the
keyring account is a stable hash of the mailbox host, username, provider,
tenant, and client ID. Set `oauth2.token_key` to choose an explicit keyring
account. If the keyring entry is removed or consent is revoked, run `auth login`
again.

For headless systems without an OS keyring, `oauth2.token_file` opts into a
JSON token file instead. This is an explicit alternative to the default
keyring, and must not be combined with `oauth2.token_key`. Mailvault creates
the file with mode `0600`, atomically replaces it when tokens rotate, and
refuses to read a file with group or world permissions. Protect and back up
this file as a credential; do not commit it.

Google's initial login still needs a browser that can reach the printed loopback
address. On a remote host, forward that loopback port over SSH before opening
the authorization URL, or authorize on a trusted local machine and securely
transfer the account-bound token file. Microsoft device login can be completed
in a browser on another device. Subsequent token refresh runs without a browser.

```yaml
oauth2:
  provider: google
  client_id: YOUR_GOOGLE_DESKTOP_CLIENT_ID
  token_file: /var/lib/mailvault/gmail-oauth.json
```

OAuth endpoints and scopes can be overridden for testing or compatible
providers. Microsoft supports `tenant`, `device_code_url`, `token_url`, and
`scopes`; Google supports `authorization_url`, `token_url`, and `scopes`.
Non-loopback endpoints must use HTTPS. Mailvault does not follow redirects for
OAuth HTTP requests.

Credential references support `env:NAME`, `file:path`, and
`keyring:service:account`. Relative `file:` paths are resolved from the
configuration file's directory. Source credential files may end with one
newline; other source password line breaks are rejected. Indexed environment
overrides include names such as `MAILVAULT_SOURCES_0_HOST` and
`MAILVAULT_SOURCES_0_OAUTH2_CLIENT_ID`.

Microsoft reference: [OAuth for IMAP and POP](https://learn.microsoft.com/en-us/exchange/client-developer/legacy-protocols/how-to-authenticate-an-imap-pop-smtp-application-by-using-oauth).
