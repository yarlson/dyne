# Live coding-session E2E test

This test runs a real Codex coding session against the private `lokalise/ratchet-test-service` repository. It builds the current dyne image, clones the repository through a GitHub App, fixes one known README link, publishes a draft pull request, verifies it through GitHub, then closes the pull request and removes its branch and Kubernetes namespace.

The test changes live GitHub and Kubernetes state and uses a real Codex account. Do not run it as an ordinary local or CI check.

## Prerequisites

Run `make doctor` and confirm that:

- the `colima-codex-proof` Docker and Kubernetes contexts are available;
- `kubectl` can reach that Kubernetes context;
- `gh` can read `lokalise/ratchet-test-service`;
- `$HOME/.codex/auth.json`, or another Codex auth file, contains valid JSON; and
- you have a GitHub App private key for an installation that can access `lokalise/ratchet-test-service`.

Check repository access:

```bash
gh auth status
gh repo view lokalise/ratchet-test-service \
  --json nameWithOwner,isPrivate,viewerPermission
```

## GitHub App configuration

The existing `lokalise-kargo` App is installed for all Lokalise repositories, including `ratchet-test-service`. Its installation grants the required repository permissions:

- Contents: Read and write
- Pull requests: Read and write

Use its verified App and installation IDs:

```bash
E2E_GITHUB_APP_ID=155561
E2E_GITHUB_INSTALLATION_ID=21035177
```

You can verify the current IDs and permissions with `gh`:

```bash
APP_SLUG=lokalise-kargo

gh api "apps/$APP_SLUG" \
  --jq '{app_id: .id, slug: .slug, owner: .owner.login, permissions: .permissions}'

gh api orgs/lokalise/installations --paginate \
  --jq ".installations[] | select(.app_slug == \"$APP_SLUG\") | {installation_id: .id, app_id, app_slug, repository_selection, permissions, html_url}"
```

If the installation command returns `403`, ask a Lokalise organization owner to run it.

For a selected-repositories installation, open the installation settings shown by its `html_url` and confirm that `ratchet-test-service` is selected. A normal `gh` OAuth token cannot call `GET /repos/lokalise/ratchet-test-service/installation`; GitHub requires an App-signed JSON Web Token for that endpoint.

GitHub does not return an App private key through the API. Use an existing `.pem` file from the App owner, or ask the App owner to generate a private key in the App settings. Keep it outside tracked paths. The `test/e2e` folder is supported because its PEM files are ignored. Restrict the key's file permissions:

```bash
chmod 600 test/e2e/lokalise-kargo.YYYY-MM-DD.private-key.pem
```

## Run the test

Create the ignored local Make configuration:

```bash
cp test/e2e/local.mk.example test/e2e/local.mk
```

Edit `test/e2e/local.mk` so `E2E_GITHUB_PRIVATE_KEY_FILE` names your downloaded `lokalise-kargo` `.pem` file. The local configuration and E2E PEM files are ignored by Git.

Run the test from the repository root:

```bash
make e2e-test
```

Command-line Make variables override `test/e2e/local.mk` when you need a one-off value.

The test stops before starting a session if the expected broken link on `main` has changed. Normal cleanup closes the draft pull request, deletes the `dyne/e2e-readme-link-*` branch, and deletes the unique `dyne-e2e-*` namespace. A hard process or machine failure can interrupt cleanup, so check GitHub and Kubernetes for those prefixes after an interrupted run.
