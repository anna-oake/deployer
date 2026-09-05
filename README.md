# deployer

A small Go service that polls a public GitHub NixOS flake and uses deploy-rs to
prepare CI-cached configurations for the hosts' next boot.

```sh
export GITHUB_REPO=anna-oake/nixos-config
export HOSTS=eule,star
export ATTIC_SERVER=attic.oa.ke
export ATTIC_CACHE=nixos
export ATTIC_TOKEN_FILE=/run/agenix/lxc-builder/deploy-attic-token
export DATA_PATH=/var/lib/deployer # optional
export INTERVAL=60s               # optional; Go duration

go build -o deployer .
./deployer
```

`git`, `nix`, and `deploy` (deploy-rs) must be in PATH. Nix must have `nix-command`
and `flakes` enabled. The repository needs a committed, complete `flake.lock` and,
for each host, `nixosConfigurations.<host>.config.system.build.toplevel` plus
`deploy.nodes.<host>.profiles.system`. The system profile must activate that same
NixOS configuration using deploy-rs's NixOS activation helper. Other profiles are
not deployed.

| Variable | Default | Meaning |
| --- | --- | --- |
| `GITHUB_REPO` | required | Public GitHub `owner/repository` |
| `DATA_PATH` | `/var/lib/deployer` | Absolute service-owned data directory |
| `HOSTS` | required | Comma-separated configuration/node names |
| `ATTIC_SERVER` | required | Attic server domain or HTTP(S) URL without a path |
| `ATTIC_CACHE` | required | Attic cache name |
| `ATTIC_TOKEN_FILE` | unset | File containing a raw Attic token; required for private caches |
| `INTERVAL` | `60s` | Positive Go duration |

A bare server domain uses HTTPS. `ATTIC_CACHE` is required, with no default.
For example, `ATTIC_SERVER=attic.oa.ke` and `ATTIC_CACHE=nixos` use
`https://attic.oa.ke/nixos/<store-path-hash>.narinfo`.

Attic's [binary cache implementation](https://github.com/zhaofengli/attic/blob/main/server/src/api/binary_cache.rs)
explicitly supports `HEAD /<cache>/<store-path-hash>.narinfo`. This service uses
HEAD only: 200 means ready, 404 means not ready yet, and other statuses are logged
as errors and retried next poll. There is no GET fallback. Readiness is a cache
presence check, not a GitHub Actions status check or a recursive closure audit.

For a private cache, `ATTIC_TOKEN_FILE` contains just the raw token (a trailing
newline is fine). The service reads it at startup and sends `Authorization:
Bearer <token>` on HEAD requests. The token needs pull access to `ATTIC_CACHE`;
it is never saved in `state.json`. Restart after rotating it. The NixOS module
requires `atticTokenFile` and passes it through a systemd credential. Nix downloads
still use Nix's own authentication, such as your existing `netrc-file` setting.

Polling starts immediately, then runs every interval. Polls never overlap; if a
poll takes longer than the interval, the next poll runs when it finishes. Each
poll fetches the remote HEAD, so default-branch changes and force pushes work.
The checkout at `$DATA_PATH/flake` is replaced if absent or associated with a
different repository. Do not put your own files there.

Successful evaluations are saved per commit and host. Missing cache entries and
failed evaluations are retried, allowing CI to finish after a commit is first
seen. Positive cache hits are saved per cache URL and store path. Successful
deployments are saved per commit and host, surviving restarts; failed deployments
are retried next poll. Each poll considers the latest default-branch commit, so
new commits supersede unfinished deployments of older ones. Hosts proceed
independently. State is saved directly to `$DATA_PATH/state.json`; one process
may use a data directory at a time. Only the current commit and configured hosts
are retained; old revisions, removed hosts and unused cache entries are pruned.
State size is bounded by the configured host count. Returning to an older commit
causes a fresh deployment.

Deployment uses the repository directly at an immutable local Git revision.
Build placement follows each host's deploy-rs configuration and Nix builder
settings. Deploy-rs runs with `--boot`,
`--magic-rollback false`, `--auto-rollback false`, `--rollback-succeeded false`, and
`--skip-checks`. It never switches the running system or reboots a host. Success
means deploy-rs completed the boot deployment, not that the host has rebooted.
Builds may substitute from configured caches; configure the Attic
substituter and its trusted public key in Nix on the machines performing builds. The CI should upload
the system closure before the toplevel becomes visible.

Unchanged polls and missing Attic paths are silent. The service logs a newly
confirmed cache entry once (remembered across restarts), then logs a successful
deployment or an actionable error. Subprocess progress is captured rather than
streamed; failures include up to 32 KiB of diagnostics. Recognizable SSH transport
failures (connection timeout/refusal, unreachable network/host) are silently
retried on the next interval. This classification uses SSH error output, not a
separate online check; authentication, host-key, DNS, build, activation, and
unrecognized failures remain visible.

Git operations time out after five minutes, each host attempt after thirty
minutes, and HTTP requests after thirty seconds. Configure noninteractive SSH
keys, known hosts, and SSH connection/liveness timeouts for the service account.
SSH options come from your deployment flake or SSH config. Cross-architecture local builds
require local emulation if the cache lacks an output that needs building.

## NixOS

The package and module live in [nix-things](https://github.com/anna-oake/nix-things),
at `packages/deployer/default.nix` and `modules/nixos/deployer.nix`.
The package's source revision and hash are pending publication of this repository.

When using nix-things' module/package discovery, enable the service in your
NixOS configuration:

```nix
{ config, ... }: {
  age.secrets."lxc-builder/deploy-attic-token" = { };
  services.deployer = {
    enable = true;
    githubRepo = "anna-oake/nixos-config";
    hosts = [ "eule" "star" ];
    atticServer = "attic.oa.ke";
    atticCache = "nixos";
    atticTokenFile = config.age.secrets."lxc-builder/deploy-attic-token".path;
    sshKeyFile = config.age.secrets.deployer-ssh-key.path;
    # interval = "60s";
    # dataPath = "/var/lib/deployer";
  };

  nix.settings = {
    extra-substituters = [ "https://attic.oa.ke/nixos" ];
    # Supply your real Attic cache public key:
    # extra-trusted-public-keys = [ "nixos:..." ];
  };
}
```

The service runs as root with Git, Nix, deploy-rs and OpenSSH in PATH. Declare your
agenix secret separately. `sshKeyFile` takes an absolute runtime path as a string,
so the private key contents stay out of the Nix store. Systemd loads the key as a
credential at service startup and a dedicated SSH agent supplies it to deploy-rs
and Nix. Use a key without a passphrase; restart the service after rotating it.
The agent exits with the service. `sshKeyFile` is required and has no default.
Keep root's known_hosts configured separately and authorize
the matching public key for the flake's deployment SSH user on each target.
If a host uses `IdentitiesOnly yes`, its SSH configuration must also identify this
key (a matching public IdentityFile is enough) so SSH will use it from the agent.
For an LXC, use a NixOS container with a functioning Nix daemon and persistent
`/var/lib/deployer`; the module does not change container or sandbox settings.
Inspect logs with `journalctl -u deployer -f`.

Build the Go service here with `go build .`. Once its source is published and
pinned, build the Nix package from nix-things with `nix build .#deployer`.
Run tests here with `go test -race ./...` and `go vet ./...`.
No Go dependencies are required outside the standard library.
