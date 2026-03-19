#!/bin/bash
# Register the Hanzo Tasks application in Hanzo IAM (hanzo.id).
#
# This creates the OIDC client that both the Tasks server (JWT validation)
# and the Tasks UI (login flow) use for SSO.
#
# Prerequisites:
#   - Access to the hanzo.id admin console
#   - Or: IAM admin API credentials
#
# The client secret must be stored in KMS at:
#   project: hanzo-k8s-epiq  path: /tasks  key: IAM_CLIENT_SECRET

set -euo pipefail

IAM_ENDPOINT="${IAM_ENDPOINT:-https://hanzo.id}"

cat <<'EOF'
=== Hanzo Tasks IAM Application Registration ===

Create (or verify) the following application in Hanzo IAM:

  Admin console:  https://hanzo.id/admin

  Application settings:
    Name:             Hanzo Tasks
    Organization:     hanzo
    Client ID:        app-tasks
    Grant Types:      authorization_code, refresh_token
    Response Types:   code
    Token Format:     JWT
    Signing Method:   RS256

  Redirect URIs:
    https://tasks.hanzo.ai/auth/sso/callback

  Scopes:
    openid
    profile
    email

  JWKS endpoint (for server-side validation):
    https://hanzo.id/.well-known/jwks
    In-cluster: http://iam.hanzo.svc/.well-known/jwks

After creating the application:

  1. Copy the Client Secret
  2. Store it in KMS:
       Project: hanzo-k8s-epiq
       Path:    /tasks
       Key:     IAM_CLIENT_SECRET
  3. The KMSSecret CRD (kms-secrets.yaml) syncs it to K8s
     as tasks-secrets/IAM_CLIENT_SECRET every 60 seconds

Verify OIDC discovery:
  curl -s https://hanzo.id/.well-known/openid-configuration | jq .

Verify JWKS:
  curl -s https://hanzo.id/.well-known/jwks | jq .keys[].kid

=== Namespace-to-Org Mapping ===

Hanzo Tasks maps Temporal namespaces to Hanzo orgs:
  - Namespace "hanzo"  -> org hanzo (default)
  - Namespace "lux"    -> org lux
  - Namespace "zoo"    -> org zoo

Users see only namespaces matching their IAM org memberships.
The Tasks UI handles this via the OIDC token's org claims.

EOF
