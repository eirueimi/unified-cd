# Identity provider setup

This page is a per-IdP recipe book for single sign-on and authorization: how to register unified-cd in your identity provider, how to make the IdP put a group or role claim into the ID token, and what to write in the controller's `oidc:` block (`roleMap` / `userMap` / `defaultRole` for the global role, `oidc.permissions` for matrix grants).

It does not repeat the permission catalogue or the resolution rules. For those, see [Authorization](authorization.md) and [Authentication](authentication.md).

> Vendor consoles change often. Menu names below are indicative and vary by product version; the **claim names, scopes and value shapes** are the load-bearing parts. Check the vendor links in each section when a menu does not match.

## How unified-cd reads identity (what every IdP must satisfy)

These points come from the controller implementation and decide what a working IdP setup looks like.

- **One issuer.** unified-cd is a relying party for exactly one `oidc.issuer`. To combine several sources (for example GitHub plus Google), put Dex in front. See [Dex as broker](#dex-as-broker).
- **Two clients.** A **confidential** web client (`clientId` / `clientSecret`) for the browser Authorization Code flow, and a **public** client without a secret (`deviceClientId`) for the CLI device flow (RFC 8628). If `deviceClientId` is unset the CLI falls back to `clientId`, which only works when the IdP lets a confidential client run the device flow without sending the secret (most do not).
- **Redirect URI.** Register exactly one URI for the web client: `<base>/api/v1/auth/oidc-callback`, where `<base>` is `oidc.externalUrl` (`UNIFIED_OIDC_EXTERNAL_URL`) if set, otherwise `http://<request host>`. Behind TLS or a reverse proxy, set `externalUrl` to the public `https://` URL so the registered URI matches. The CLI device flow sends no redirect URI of its own.
- **Claims come from the ID token only.** The controller reads the roles claim from the verified ID token. It does not call the `userinfo` endpoint and does not read the access token. A claim that the IdP only exposes through `userinfo` or the access token is invisible to unified-cd.
- **Top-level claim name only.** `rolesClaim` is looked up as a single key of the ID token. A dotted name such as `realm_access.roles` is treated as one literal key and will not match the nested JSON object. Namespaced URL claims (Auth0) are top-level keys and work. The default is `groups`.
- **Accepted value shapes.** A JSON array of strings, or a single string, which is treated as one value. A comma- or space-separated string is **not** split: `"a b"` is the one value `a b`. Non-string array elements are dropped.
- **Scopes requested.** `openid`, `email`, `profile`, plus `rolesClaim` itself as an extra scope when the claim name contains neither `/` nor `:` (so `groups` adds the scope `groups`, `roles` adds the scope `roles`; a namespaced Auth0 claim adds nothing). The CLI reads the same list from `GET /api/v1/auth/oidc-config` (`scopes`). There is no separate setting to add or remove scopes. If your IdP rejects an unknown scope, see the per-IdP notes.
- **Identity matching.** `userMap` and `permissions.*.users` match the token's `email` and its `sub`. If the token has no `email`, the controller uses `sub` in its place. The `email_verified` claim is **not** checked, so only use IdPs where users cannot choose an unverified email address of their own.
- **Group changes** reach unified-cd at the next browser login (the ID token is re-issued then) and on silent session refresh when the refreshed ID token still carries the claim. CLI id_tokens carry the groups from login time until they expire.

## Which topology to choose

| Situation | Recommendation |
|---|---|
| One OIDC-compliant IdP that can emit a flat top-level `groups`/`roles` claim in the ID token (Okta, Keycloak, Auth0, Entra with App Roles) | Direct to the IdP. |
| GitHub | Dex only. GitHub is not an OIDC provider. |
| Google Workspace groups | Dex only (Google emits no groups claim). |
| Several sources at once, or you want to normalise group value shapes | Dex. |
| GitLab, when you need ancestor groups or the device flow | Dex (see the GitLab section). |

## Dex as broker

[Dex](https://dexidp.io/) is an OIDC provider that federates other identity sources through connectors and issues its own ID tokens. unified-cd points `oidc.issuer` at Dex; each connector builds the `groups` claim for its source. The repository ships a development Dex setup (`dex-config.sso.yaml`, `docker-compose.sso.yml`); see [Authentication](authentication.md#sso-setup-oidc-dex).

A production Dex needs two static clients (or an equivalent client store):

```yaml
# dex config (excerpt)
issuer: https://dex.example.com
staticClients:
  - id: unified-cd
    name: unified-cd
    secret: <web-client-secret>
    redirectURIs:
      - https://unified-cd.example.com/api/v1/auth/oidc-callback
  - id: unified-cd-cli
    name: unified-cd CLI
    public: true
    redirectURIs:
      - https://dex.example.com/device/callback
```

```yaml
# unified-cd controller config
oidc:
  issuer: https://dex.example.com
  externalUrl: https://unified-cd.example.com
  clientId: unified-cd
  clientSecret: <web-client-secret>
  deviceClientId: unified-cd-cli
  rolesClaim: groups
```

Notes:

- Dex only emits `groups` when the client requests the `groups` scope. unified-cd does that automatically while `rolesClaim` is `groups`.
- Dex's device-flow callback is `/device/callback` on the Dex host (in the bundled development setup the controller proxies `/dex/*` and `/device/callback`). Keep the public client's `redirectURIs` in line with the Dex version you run.
- Dex reads its configuration at startup; restart it after editing.
- Group value shape depends on the connector. The sections below give the shape per connector.

---

## Microsoft Entra ID

**Value shapes.** The `groups` claim carries group **object IDs (GUIDs)** by default. App Roles arrive in the `roles` claim as the role's **value** string (for example `team-a`).

**Recommended: App Roles** (small, stable, readable, and immune to the groups overage described below).

IdP side (menu names vary by version):

1. In the Entra admin center, **App registrations > New registration**. Platform **Web**. Redirect URI: `https://unified-cd.example.com/api/v1/auth/oidc-callback`. Create a client secret under **Certificates & secrets**.
2. Under **App roles**, create roles for users and groups (for example values `admin`, `team-a`, `team-b`).
3. In **Enterprise applications > (your app) > Users and groups**, assign users or groups to those roles. Assigned app roles appear in the ID token's `roles` claim.
4. For the CLI, create a second registration (or enable **Allow public client flows** on a registration) and use its application ID as `deviceClientId`. Entra's device code flow needs a public client.

unified-cd config:

```yaml
oidc:
  issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
  externalUrl: https://unified-cd.example.com
  clientId: <web-application-id>
  clientSecret: <client-secret>
  deviceClientId: <cli-application-id>
  rolesClaim: roles
  roleMap:
    admin: admin
  defaultRole: deny
  permissions:
    folders:
      team-a:
        groups:
          team-a: developer
      team-b:
        groups:
          team-b: developer
```

**Alternative: groups claim.** Under **Token configuration > Add groups claim** choose the group types. For tenants with many groups prefer **Groups assigned to the application**. Values are GUIDs, so:

```yaml
oidc:
  rolesClaim: groups
  permissions:
    folders:
      team-a:
        groups:
          "11111111-2222-3333-4444-555555555555": developer   # object ID of the team-a group
```

Gotchas:

- **Groups overage.** A JWT carries at most 200 groups. Beyond that Entra drops the `groups` claim and puts a link in `_claim_names` / `_claim_sources` instead. unified-cd does not follow that link, so the user appears to have no groups. Use App Roles, or limit the claim to groups assigned to the application (see [Entra optional claims](https://learn.microsoft.com/en-us/entra/identity-platform/optional-claims)).
- **GUIDs vs names.** Without extra configuration the claim contains GUIDs, not names. Comment your config with the group names. Entra can emit on-premises `sAMAccountName` values for synced groups through the `groups` optional claim, and cloud group display names with the `cloud_displayname` option together with "groups assigned to the application". Prefer GUIDs or App Roles unless you need names.
- **Scope name (verify).** unified-cd adds `rolesClaim` as a scope (`roles` or `groups`) because it is a bare word. Entra does not define scopes with those names; whether the v2.0 endpoint ignores or rejects them was not verified here. If login fails with an invalid-scope error, put Dex in front with the generic `oidc` connector (`insecureEnableGroups: true`, and map the Entra claim to Dex groups; the claim-mapping option names depend on the Dex version) and set `rolesClaim: groups` in unified-cd. Test in a non-production tenant first.
- Users without an assignment get no `roles` claim; `defaultRole: deny` then refuses them unless a `permissions` entry matches.

## Okta

**Value shape.** Group **names** (for example `team-a`), as filtered by the claim you configure.

IdP side:

1. **Applications > Create App Integration > OIDC - OpenID Connect > Web Application.** Sign-in redirect URI: `https://unified-cd.example.com/api/v1/auth/oidc-callback`. Assign the app to the users or groups that may sign in.
2. Add the groups claim to the ID token:
    - **Org authorization server** (issuer `https://<org>.okta.com`): on the app's **Sign On** tab, edit **OpenID Connect ID Token**, keep the claim name `groups` and set a filter (for example matches regex `team-.*`). The `groups` scope must be requested; unified-cd does so when `rolesClaim` is `groups`.
    - **Custom authorization server** (for example issuer `https://<org>.okta.com/oauth2/default`): **Security > API > (server) > Claims > Add Claim**, name `groups`, include in **ID Token** (always), value type **Groups**, filter by regex. A custom server also needs a scope named `groups` (**Scopes** tab) because unified-cd always requests it.
3. For the CLI: create a second app integration of type **Native** (public, no secret) and enable the **Device Authorization** grant type. Its client ID is `deviceClientId`. Add the same groups claim for that app (or server-wide on the custom server).

unified-cd config:

```yaml
oidc:
  issuer: https://example.okta.com/oauth2/default
  externalUrl: https://unified-cd.example.com
  clientId: <web-client-id>
  clientSecret: <client-secret>
  deviceClientId: <native-client-id>
  rolesClaim: groups
  roleMap:
    platform-admins: admin
  defaultRole: deny
  permissions:
    folders:
      team-a:
        groups:
          team-a: developer
      team-a/prod:
        groups:
          team-a-sre: [job.read, run.approve]
```

Gotchas:

- The groups claim **filter** decides which groups appear. A group missing from the filter silently matches nothing.
- Groups must be in the ID token itself. Okta documents that some flows return a thin ID token without groups (you would then need `userinfo`, which unified-cd does not read); the authorization code flow returns the full token. Verify with a test login by decoding the ID token.
- Vendor reference: [Customize tokens with a groups claim](https://developer.okta.com/docs/guides/customize-tokens-groups-claim/main/).

## Keycloak

**Value shapes.** Depends on the mapper. A **Group Membership** mapper emits group paths with **Full group path** on (`/team-a`, `/org/team-a`) or just names with it off (`team-a`). Realm roles are nested by default and need a flat mapper.

IdP side:

1. In your realm, **Clients > Create client**, client type OpenID Connect. Turn **Client authentication** on (confidential). Valid redirect URI: `https://unified-cd.example.com/api/v1/auth/oidc-callback`. Copy the secret from **Credentials**.
2. Create the group claim: on the client (or in a shared client scope), **Mappers > Add mapper > By configuration > Group Membership**. Token claim name `groups`, **Add to ID token** on. Decide **Full group path** now: it changes the values you write in the controller config.
3. To use realm roles instead of groups, add a **User Realm Role** mapper (multivalued) with token claim name `roles` and **Add to ID token** on, and set `rolesClaim: roles`. Do not rely on the default `realm_access.roles` claim: it is nested and `rolesClaim` cannot reach it.
4. CLI: create a second client with **Client authentication off** (public) and the **OAuth 2.0 Device Authorization Grant** enabled, and add the same mapper to it. Its ID is `deviceClientId`.

unified-cd config (Full group path **on**):

```yaml
oidc:
  issuer: https://keycloak.example.com/realms/engineering
  externalUrl: https://unified-cd.example.com
  clientId: unified-cd
  clientSecret: <client-secret>
  deviceClientId: unified-cd-cli
  rolesClaim: groups
  roleMap:
    /platform: admin
  defaultRole: deny
  permissions:
    folders:
      team-a:
        groups:
          /teams/team-a: developer
      team-b:
        groups:
          /teams/team-b: developer
```

Gotchas:

- `/team-a` and `team-a` are different values. Changing the mapper's **Full group path** switch changes every group key; re-check with a decoded token.
- Keycloak lists the member's own group path (for example `/teams/team-a`); do not assume parent groups are included.
- unified-cd requests a scope named `groups` when `rolesClaim` is `groups`. The mapper does not depend on it, but verify that your Keycloak version does not reject a scope it does not know.
- Use the realm's issuer URL exactly as shown by its discovery document.

## Google Workspace

**There is no groups claim in Google ID tokens.** Direct sign-in gives you `email`, `email_verified`, `sub` and (for Workspace accounts) `hd`. Two options:

- **Direct, per user.** Use `userMap` and `permissions.*.users` keyed by email.
- **Groups through Dex.** Dex's `google` connector can fetch Workspace groups with a service account that has domain-wide delegation and an admin email to impersonate.

IdP side (direct):

1. In Google Cloud Console, **APIs & Services > Credentials > Create OAuth client ID**, type **Web application**. Authorized redirect URI: `https://unified-cd.example.com/api/v1/auth/oidc-callback`.
2. Restrict sign-in to your organisation, for example by setting the OAuth consent screen user type to **Internal**.
3. CLI device flow: Google's device flow uses a separate "TVs and limited-input devices" client type that has a client secret, and allows only a small scope set. unified-cd's public-client device flow sends no secret, so use Dex in front if you need `unified-cli login` with Google. This was not verified end to end.

Direct config (grants by user email only):

```yaml
oidc:
  issuer: https://accounts.google.com
  externalUrl: https://unified-cd.example.com
  clientId: <client-id>.apps.googleusercontent.com
  clientSecret: <client-secret>
  defaultRole: deny
  userMap:
    alice@example.com: admin
  permissions:
    folders:
      team-a:
        users:
          bob@example.com: developer
          carol@example.com: [job.read, run.trigger]
```

Via Dex (groups):

```yaml
# dex connector
connectors:
  - type: google
    id: google
    name: Google
    config:
      clientID: <client-id>.apps.googleusercontent.com
      clientSecret: <client-secret>
      redirectURI: https://dex.example.com/callback
      serviceAccountFilePath: /etc/dex/google-sa.json   # domain-wide delegation
      adminEmail: admin@example.com                     # an admin to impersonate
      groups:
        - team-a@example.com
        - team-b@example.com
```

```yaml
# unified-cd (issuer is Dex; see "Dex as broker")
oidc:
  rolesClaim: groups
  permissions:
    folders:
      team-a:
        groups:
          team-a@example.com: developer
```

Gotchas: the Dex Google connector's option names (`serviceAccountFilePath`, `adminEmail`, `groups`, `domainToAdminEmail`) differ between Dex releases; check the [Dex Google connector docs](https://dexidp.io/docs/connectors/google/) for your version. Group values are the groups' email addresses.

## GitHub (through Dex)

GitHub is an OAuth provider, not an OIDC provider (no ID token), so it can only be used through the Dex `github` connector. Team membership becomes the `groups` claim.

IdP side:

1. In GitHub, create an **OAuth App** (organisation settings > Developer settings > OAuth Apps). Authorization callback URL: `https://dex.example.com/callback` (Dex's callback, not unified-cd's).
2. Grant the app access to your organisation (org owners may need to approve it, or it cannot read private team membership).
3. Configure the Dex connector, then the Dex clients as in [Dex as broker](#dex-as-broker).

```yaml
# dex connector
connectors:
  - type: github
    id: github
    name: GitHub
    config:
      clientID: <github-oauth-client-id>
      clientSecret: <github-oauth-client-secret>
      redirectURI: https://dex.example.com/callback
      orgs:
        - name: my-org
      teamNameField: slug
```

```yaml
# unified-cd
oidc:
  issuer: https://dex.example.com
  externalUrl: https://unified-cd.example.com
  clientId: unified-cd
  clientSecret: <web-client-secret>
  deviceClientId: unified-cd-cli
  rolesClaim: groups
  roleMap:
    "my-org:platform": admin
  defaultRole: deny
  permissions:
    folders:
      team-a:
        groups:
          "my-org:team-a": developer
      team-a/prod:
        groups:
          "my-org:team-a-sre": [job.read, run.approve]
```

Gotchas:

- Values are `org:team-slug`. Quote them in YAML because of the colon.
- Without `teamNameField: slug` Dex emits the team's display name, which can contain spaces and capitals and changes when someone renames the team.
- Only organisations listed under `orgs` are consulted; users outside them are refused by Dex.
- Do not assume membership of a child team implies its parent team.

## GitLab

**Value shape.** Group paths: `group` or `group/subgroup`.

GitLab (gitlab.com and self-managed) is an OIDC provider. According to [GitLab's documentation](https://docs.gitlab.com/integration/openid_connect_provider/) the `groups_direct` claim (paths of groups the user is a **direct** member of) is in the ID token, while `groups` (including ancestor groups) is only in `userinfo`. unified-cd reads the ID token only, so direct use means `rolesClaim: groups_direct`.

IdP side (direct):

1. GitLab **Applications** (user, group or instance level). Confidential application, redirect URI `https://unified-cd.example.com/api/v1/auth/oidc-callback`, scopes `openid`, `email`, `profile`. Copy the application ID and secret.

Direct config:

```yaml
oidc:
  issuer: https://gitlab.example.com
  externalUrl: https://unified-cd.example.com
  clientId: <application-id>
  clientSecret: <secret>
  rolesClaim: groups_direct
  roleMap:
    platform: admin
  defaultRole: deny
  permissions:
    folders:
      team-a:
        groups:
          mycompany/team-a: developer
```

Gotchas:

- **Direct members only.** A user in `mycompany/team-a/sub` is not reported as a member of `mycompany/team-a` through `groups_direct`. Grant the exact groups users belong to, or use Dex.
- **Scope and device flow (verify).** unified-cd adds `rolesClaim` (`groups_direct`) as a scope. GitLab's documented OIDC scopes do not include it, and device-flow support for OIDC applications depends on the GitLab version; neither was verified here. If either is a problem, use the Dex `gitlab` connector instead.
- **Via Dex:** the `gitlab` connector emits the user's group paths as `groups`; set `rolesClaim: groups` in unified-cd.

```yaml
# dex connector
connectors:
  - type: gitlab
    id: gitlab
    name: GitLab
    config:
      baseURL: https://gitlab.example.com
      clientID: <application-id>
      clientSecret: <secret>
      redirectURI: https://dex.example.com/callback
      groups:
        - mycompany          # optional: restrict to these groups
```

## Auth0

**Value shape.** Whatever your Action writes; role names are the usual choice. Auth0 does not put roles in the ID token by itself, so a Post-Login Action adds them under a **namespaced claim** (a URL-style name that you own).

IdP side:

1. **Applications > Create Application > Regular Web Application.** Allowed Callback URL: `https://unified-cd.example.com/api/v1/auth/oidc-callback`.
2. Create roles (**User Management > Roles**) and assign them to users.
3. **Actions > Library > Build Custom**, trigger Login / Post Login, add the roles to the ID token, then attach the action to the Login flow:

    ```js
    exports.onExecutePostLogin = async (event, api) => {
      const ns = "https://unified-cd.example.com/";
      api.idToken.setCustomClaim(ns + "roles", event.authorization?.roles ?? []);
    };
    ```

4. CLI: create a **Native** application (public) and enable the **Device Code** grant type in its advanced settings. Its client ID is `deviceClientId`. The same Action applies tenant-wide.

unified-cd config:

```yaml
oidc:
  issuer: https://example.us.auth0.com/     # keep the trailing slash
  externalUrl: https://unified-cd.example.com
  clientId: <web-client-id>
  clientSecret: <client-secret>
  deviceClientId: <native-client-id>
  rolesClaim: "https://unified-cd.example.com/roles"
  roleMap:
    admin: admin
  defaultRole: deny
  permissions:
    folders:
      team-a:
        groups:
          team-a-developers: developer
```

Gotchas:

- The issuer must match the `iss` claim character for character, including the trailing `/`.
- A claim name containing `/` or `:` is not added as a scope, so no extra scope is requested.
- Auth0 requires custom ID-token claims to be namespaced; that is why the claim is a URL.

---

## Checklist

1. **Decode a real ID token** (from a test login) and confirm the claim is present at the top level, is an array of strings, and has the exact value shape you wrote in the config.
2. **Start the controller.** A bad `oidc.permissions` block stops startup; the error strings (unknown permission, global-only permission on a folder, bad folder key, no issuer) are listed in [Authorization: Startup validation](authorization.md#startup-validation). Also read the startup warnings in [Operations](operations.md#authorization-startup-warnings).
3. **Explain before you trust.** For a sample user:

    ```bash
    unified-cli authz explain --user dev@example.com --group my-org:team-a --job team-a/build
    ```

    See [Inspecting the policy](authorization.md#inspecting-the-policy-authz-show-and-authz-explain). It describes an identity, not a token.
4. **Log in again.** Sessions store the groups captured at login; users must sign in again (browser and `unified-cli login`) after an IdP-side claim change.
5. **Re-issue role-only PATs** when you introduce folders: they have no owner and bypass folder grants until revoked. See [PAT owners and ceilings](authentication.md#pat-owners-and-ceilings).
6. **Confirm denial works.** A user that matches nothing should get `403 forbidden: no role assigned` (browser) or `403 forbidden: no role` (bearer token). If everyone is denied, the claim is usually missing from the ID token or its values do not match your keys.

Troubleshooting entries for the login and token errors are in [Authentication](authentication.md#troubleshooting).
