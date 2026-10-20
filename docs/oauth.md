# Microsoft OAuth for IMAP and POP

Mailvault uses Microsoft's device authorization flow for Outlook.com/Hotmail
accounts. Register a public client in Microsoft Entra, enable public client
flows, and grant the delegated `IMAP.AccessAsUser.All` and
`POP.AccessAsUser.All` permissions. Mailvault requests the matching Outlook
protocol scopes plus `offline_access`, so it can refresh access tokens without
repeating device login. Supply the app's client ID; Mailvault does not ship an
application ID or require a client secret.

Configure the source with `oauth2.client_id` and omit `password_ref`:

```yaml
sources:
  - name: outlook
    type: imap
    host: outlook.office365.com
    port: 993
    username: user@outlook.com
    use_tls: true
    oauth2:
      client_id: YOUR_MICROSOFT_PUBLIC_CLIENT_ID
      tenant: consumers
```

Authorize the source once with:

```sh
mailvault --config ./config.yaml auth login --source outlook
```

Open the displayed Microsoft verification page, enter the one-time code, and
complete sign-in. Device codes and tokens are never saved in the config file or
printed by Mailvault. The OS keyring stores the token set under service
`mailvault`. By default, the keyring account is a stable hash of the mailbox
host, username, tenant, and client ID; set `oauth2.token_key` to choose an
explicit keyring account. If the keyring entry is removed or consent is
revoked, run `auth login` again.

The identity endpoints default to Microsoft's `consumers` endpoints. `tenant`,
`device_code_url`, `token_url`, and `scopes` can be set for a different tenant
or for a local test server. Non-loopback endpoints must use HTTPS. Redirects are
not followed for device or token requests.

Credential references support `env:NAME`, `file:path`, and
`keyring:service:account`. Relative `file:` paths are resolved from the
configuration file's directory. Source credential files may end with one
newline; other source password line breaks are rejected. Indexed environment
overrides use names such as `MAILVAULT_SOURCES_0_HOST` and
`MAILVAULT_SOURCES_0_OAUTH2_CLIENT_ID` and apply to entries declared in the
configuration file.
