# localrectl

**`localrectl` is the command-line client of [local-registry](https://github.com/nivekalara237/local-registry).**

local-registry is a private server where your teams store the versions of their internal deliverables: binaries, archives, configuration bundles, any file. `localrectl` talks to this server for you, from your terminal or from a CI pipeline.

- local-registry repository: https://github.com/nivekalara237/local-registry
- local-registry documentation: https://local-registry.nivekaa.com

## Table of contents

- [What localrectl does](#what-localrectl-does)
- [Words to know](#words-to-know)
- [Install](#install)
- [Getting started](#getting-started)
- [Configuration](#configuration)
- [Commands](#commands)
  - [ping](#ping-check-the-server)
  - [validate](#validate-check-a-product-card)
  - [products](#products-find-a-product)
  - [versions](#versions-list-the-versions-of-a-product)
  - [release](#release-read-a-version)
  - [publish](#publish-send-a-new-version-coming-soon)
  - [Other commands](#other-commands)
- [Exit codes](#exit-codes)
- [Security](#security)
- [Contributing](#contributing)
- [License](#license)

## What localrectl does

`localrectl` covers the two sides of local-registry:

- **Reading**: find a product, see its versions, read what a version holds. No token needed.
- **Publishing**: check a product card, then send a new version to the server. Sending needs an upload token.

## Words to know

- **Product**: something your team delivers, such as `billing-agent`. A product has a name and many versions.
- **Version**: one release of a product, such as `v1.2.0`. Once published, a version never changes.
- **Latest**: the version the product owners chose as the current one.
- **Prerelease**: a test version, such as `v2.0.0-rc.1`. It is hidden unless you ask for it.
- **Product card**: the `registry.yaml` file that describes a product (display name, description, owners, keywords…).
- **`release.json`**: the file the server writes for each version: its files, their sha256, its date…
- **Upload token**: the secret password the server asks for before it accepts new files.

## Install

You need [Go](https://go.dev/dl/) 1.27 or later.

```sh
git clone https://github.com/nivekalara237/localrectl.git
cd localrectl
go build -o localrectl ./cmd
```

Move the `localrectl` file to a folder of your `PATH`, such as `/usr/local/bin`. Then check that it works:

```sh
localrectl version
```

## Getting started

**1. Tell `localrectl` where your server is.** Without this step, it uses `http://localhost:8077`.

```sh
export LOCAL_REGISTRY_URL=https://registry.example.com
```

**2. Check that the server answers.**

```sh
localrectl ping
```

**3. Find a product, then its versions, then the content of a version.**

```sh
localrectl products --query billing
localrectl versions billing-agent
localrectl release billing-agent latest
```

**4. Before you publish, check your product card.**

```sh
localrectl validate registry.yaml
```

## Configuration

Each setting can come from four places. When a setting is given in several places, the first one of this list wins:

1. a flag on the command line, such as `--server-url`
2. an environment variable, such as `LOCAL_REGISTRY_URL`
3. the config file
4. the default value

| Flag | Environment variable | Config file key | Default | What it does |
|---|---|---|---|---|
| `--server-url` | `LOCAL_REGISTRY_URL` | `server.url` | `http://localhost:8077` | Address of the server |
| | `LOCAL_REGISTRY_UPLOAD_TOKEN` | `server.upload-token` | | Upload token, needed only to publish |
| `--server-ca-file` | `LOCAL_REGISTRY_CA_FILE` | `server.ca-file` | | Certificate of your company, for an internal HTTPS server |
| `--config` | `LOCAL_REGISTRY_CONFIG` | | | Config file to read |
| `-o, --output` | `LOCAL_REGISTRY_OUTPUT` | `output` | `text` | `text` to read it, `json` for scripts and `jq` |
| `-v, --verbose` | `LOCAL_REGISTRY_VERBOSE` | `verbose` | `false` | Show the details: config file used, requests sent… |
| `-q, --quiet` | `LOCAL_REGISTRY_QUIET` | `quiet` | `false` | Show only the result and the errors |
| `--no-color` | `LOCAL_REGISTRY_NO_COLOR` | `no-color` | `false` | No colors |
| `--timeout` | `LOCAL_REGISTRY_TIMEOUT` | `timeout` | `300` | Time limit of a request, in seconds |
| `--retries` | `LOCAL_REGISTRY_RETRIES` | `retries` | `3` | Retries after a network or server error |
| `--exit-code` | `LOCAL_REGISTRY_EXIT_CODE` | `exit-code` | `1` | Exit code of a failed command (see [Exit codes](#exit-codes)) |

### The config file

Without `--config`, `localrectl` looks for a file named `config`, `config.yaml` or `config.yml` in these folders, in this order: `/etc/localrectl/`, `~/.lr/`, then the current folder. It reads the first one it finds.

```yaml
server:
  url: https://registry.example.com
  upload-token: "your-token"
  ca-file: ~/certs/company-ca.pem
output: text
timeout: 300
retries: 3
```

Always write the token between quotes: without them, a token made only of digits is read as a number and refused.

To see the settings `localrectl` uses, and where each one comes from:

```sh
localrectl config
```

## Commands

Every command accepts `--help`, which shows all its options and more examples.

### `ping`: check the server

Checks that the server answers, and tells you whether an upload token is set.

```sh
localrectl ping
```

### `validate`: check a product card

Checks a `registry.yaml` file against the rules of the server, so you find mistakes before you publish. The file is read on your machine; only the rules are downloaded from the server.

Add `--artifacts` to also check the `artifacts.json` file that goreleaser writes.

```sh
localrectl validate registry.yaml
localrectl validate registry.yaml --artifacts dist/artifacts.json
```

### `products`: find a product

Without an option, lists every product of the server. Each option narrows the search, and a product must match all of them.

```sh
localrectl products
localrectl products --query billing
localrectl products --keyword vcs --keyword plugin
localrectl products --label team=devtools --owner devtools@example.com
localrectl products --updated-since 2026-01-01
```

- `-Q, --query TEXT`: a text to find in the name, the display name or the description
- `-k, --keyword K`: a keyword of the product (repeat it to ask for several)
- `-l, --label key=value`: a label of the product (repeat it to ask for several)
- `--owner O`: a name or an email of the owners
- `--license L`: a license, such as `MIT` or `Apache-2.0`
- `--deprecated`: only the deprecated products; `--deprecated=false` for the others
- `--prerelease`: only the products that have a prerelease; `--prerelease=false` for the others
- `--updated-since DATE`: products updated on or after this date, such as `2026-01-31`
- `--sort KEY`: the order of the list
- `--limit N`: how many products per page

The server answers one page at a time. When more pages exist, `localrectl` tells you how to get the next one:

- `-c, --cursor C`: read the page `C`
- `--all`: read every page at once

```sh
localrectl products --limit 20
localrectl products --limit 20 --cursor <next>
localrectl products --all
```

### `versions`: list the versions of a product

Lists the published versions of a product, and shows which one is the latest and which ones are deprecated. Add `--prerelease` to also see the test versions.

```sh
localrectl versions billing-agent
localrectl versions billing-agent --prerelease
```

### `release`: read a version

Shows the `release.json` of a version: its files, their sha256 and its details. Write `latest` instead of a version number to read the latest one.

```sh
localrectl release billing-agent v1.2.0
localrectl release billing-agent latest
```

### `publish`: send a new version (coming soon)

> **Not available yet.** Until then, use `scripts/upload.sh` from the [local-registry repository](https://github.com/nivekalara237/local-registry).

`publish` sends the files of a new version to the server, then makes the version public. It needs the upload token.

```sh
localrectl publish <product> [artifacts.json | FOLDER]
```

You can publish in two ways:

- **From goreleaser**: `localrectl` reads the `artifacts.json` file that goreleaser writes, and sends the files listed in it. Without a path, it uses `dist/artifacts.json`.
- **From a folder**: `localrectl` sends every file of the folder. The folder cannot hold subfolders: put them in an archive first, with `tar czf`.

```sh
# After goreleaser: publish and make it the latest version
localrectl publish billing-agent --latest

# A folder of files, with its version number
localrectl publish billing-agent ./out --version v1.2.0

# See what would be sent, without sending anything
localrectl publish billing-agent --dry-run
```

What happens, step by step:

1. `localrectl` checks the product card and `artifacts.json` on your machine.
2. It sends each file, with its sha256 so the server can check it.
3. It sends the product card (`registry.yaml`) and `CHANGELOG.md`, then `artifacts.json`.
4. It asks the server to publish. The version appears whole, or not at all.

Options:

- `--latest`: also make this version the latest one
- `--dry-run`: show what would be sent, and send nothing (no token needed)
- `--version V`: the version number; by default, the one found in goreleaser's `metadata.json`
- `--product-file F`: the product card to send; by default, the `registry.yaml` of the folder
- `--root DIR`: the folder that the paths of `artifacts.json` start from
- `--changelog F`: the `CHANGELOG.md` to send; by default, the one next to `artifacts.json`
- `--exclude GLOB`: files of the folder not to send, such as `*.log` (repeat it to give several)
- `--parallel N`: how many files are sent at the same time (4 by default)
- `--no-validate`: skip the check of step 1
- `--if-exists fail|skip`: if the version already exists, fail (the default), or stop without an error, so a CI job can run again

### Other commands

- `localrectl config`: show the settings in use, and where each one comes from
- `localrectl version`: show the version of `localrectl`
- `localrectl completion bash|zsh|fish|powershell`: print the auto-completion script for your shell

## Exit codes

When a command ends, it returns a number to the shell. A CI pipeline uses it to know whether the step failed.

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Network or server error |
| 2 | Bad use of the command: unknown option, missing argument, wrong setting |
| 3 | Validation refused (by the server, or by the local check) |
| 4 | The version already exists |
| 5 | A sha256 or a signature does not match |
| 6 | Not found |

Codes 3 to 6 come with `publish` and `download`. Today, a failed command returns `1`, or the number you give to `--exit-code`:

```sh
localrectl validate registry.yaml --exit-code 3
```

## Security

- **Never type the upload token on the command line.** Anyone on the machine can see it with `ps`, and it stays in the history of your shell. Put it in the `LOCAL_REGISTRY_UPLOAD_TOKEN` variable, or in the config file.
- **Protect the config file that holds the token**, so that only you can read it: `chmod 600 ~/.lr/config`. `localrectl` warns you when other users can read it.
- **`localrectl` never shows the token**, not even with `--verbose` or `localrectl config`.
- **The token is only for publishing.** Reading products and versions needs no token.
- **To report a vulnerability**, do not open a public issue: use a [private security advisory](https://github.com/nivekalara237/localrectl/security/advisories/new).

## Contributing

Issues and pull requests are welcome. Before you open a pull request, check that this command passes:

```sh
gofmt -l . && go vet ./... && go test -race ./...
```

## License

`localrectl` is under the MIT license: see [LICENSE](LICENSE).
