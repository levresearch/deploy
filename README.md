## Deploy

a client to dynamically deploy managed services on servers you own

### requirements:
**your machine**
on the machine you're running this on, you'll need `git`, `docker`, and `ssh` installed. ssh is not a hard requirement but if you're deploying on something that isn't your host machine, which is like 99% of the people using this, you'll want ssh.

**your server**
you just want `docker` installed, if you're exposing the project through a tunnel using our host config, you'll want `cloudflared` installed too

if either machine is missing something deploy needs, it'll tell you when you run it.

### to use:
```bash
cd ~/project # needs to be a git project, git init if not already
deploy # inits deploy and creates config file

# once you complete your deploy config, run it again
deploy
```

### examples:

simple web server (accessible on 8080):
```jsonc
{
  "version": 1, // schema ver
  "id": "xxxxxxxx", // how deploy identifies your project
  "name": "example-project",
  "destination": "x.x.x.x:/srv/projects", // where to deploy to, x.x.x.x is the ip of the server you're deploying to, follows ssh path syntax
  "services": {
    "app": {
      "image": "nginx:alpine",
      "stateful": false,
      "ports": ["8080:80"],
      "healthcheck": { "command": ["CMD", "wget", "-qO-", "http://localhost/"] }
    }
  }
}
```

multi env, running deploy with -e (env name) will deploy that specific environment.:
```jsonc
{
  "version": 1,
  "id": "d4e5f6a7",
  "name": "shop",
  "services": {
    "web": {
      "build": { "dockerfile": "Dockerfile" },
      "stateful": false,
      "healthcheck": { "command": ["CMD", "curl", "-f", "http://localhost:3000/health"] }
    }
  },
  "environments": {
    "production": {
      "services": {
        "web": {
          "env": [".env.production"],
          "host": { "domain": "shop.example.com", "port": 3000, "tunnelTokenFrom": "TOKEN" }
        }
      }
    },
    "staging": {
      "services": {
        "web": { "env": [".env.staging"], "ports": ["3000:3000"] }
      }
    }
  }
}
```

so if you want to deploy only what's in staging, you run deploy -e staging or deploy --environment staging. 

deploy automatically deploys in prod btw

production keeps its original deployment names. other environments use their own
project-id/environment identity for state, secrets, containers, networks, named
volumes and image tags. environment names must be lowercase slugs, at most 32
characters. explicit bind mounts, published ports and tunnel domains still need
separate configuration when the environments run on the same host.

old deployments recorded under a different environment are refused, not moved.
resolve their ownership and transition their data explicitly before deploying.
stop all older cli processes before upgrading: they do not understand the new
lock. the destination needs `flock` (util-linux); mutating commands hold a live
exclusive lock, including destroy and env push. never delete `.deploy-locks/`
while a command is running. `--force-unlock` can clear abandoned legacy metadata
but cannot bypass a live lock.

before changing anything, each of those commands records what it is doing in
`.deploy-locks/<project>.operation.json`, and removes the record once it has seen
how the command ended, whether it succeeded or failed. a crash, ctrl-c, lost
connection, or a release that is live but could not be recorded leaves the record
behind, because a remote command can outlive the session that started it. the
next mutating command then refuses with the operation's id. check that nothing
from it is still running, inspect the deployment with `deploy status` and
`deploy releases`, then run `deploy resolve <id>`. resolving clears the record
only; it does not undo or repeat anything, and `--force-unlock` does not skip it.

state writes require GNU coreutils (`mktemp`, `mv`, and `sync`) on the destination.
deploy and rollback check these tools before changing containers. state is written
to a private temporary file in the same directory, checked for a complete transfer,
flushed, and atomically renamed. the directory and its ancestors are then flushed
so newly created project directories are persisted too. existing unversioned state
remains readable; new writes identify state format 1. unsupported state versions
are refused.

if state persistence fails after a rename or the SSH connection is lost, the write
may already have happened. inspect state and running containers before retrying;
an error is not proof that a release task or deployment did not run. this storage
change does not yet add an operation journal or disconnect-safe mutation ownership.

database, and anything else that holds data you care about:
```jsonc
{
  "version": 1,
  "id": "xxxxxxxx",
  "name": "shop",
  "destination": "x.x.x.x:/srv/projects",
  "services": {
    "web": {
      "build": { "dockerfile": "Dockerfile" },
      "stateful": false, // gets replaced every deploy
      "env": [".env.production"], // pushed with deploy env push, never committed
      "healthcheck": { "command": ["CMD", "curl", "-f", "http://localhost:3000/health"] }
    },
    "pg": {
      "image": "postgres:17",
      "stateful": true, // started once, left alone after that
      "volumes": ["pgdata:/var/lib/postgresql/data"], // your data lives here
      "env": [".env.production"],
      "healthcheck": { "command": ["CMD-SHELL", "pg_isready -U postgres"], "retries": 15 }
    }
  },
  "release": {
    "migrate": { // runs before the new code starts, against the live db
      "build": { "dockerfile": "Dockerfile" },
      "command": "npm run db:migrate",
      "env": [".env.production"]
    }
  }
}
```

anything that owns data is `stateful: true`, so postgres, redis, a minecraft world, anything with a volume you'd be upset to lose. everything else is `stateful: false` and gets a fresh copy every commit, which is what lets the new version start up next to the old one and take over without dropping any traffic.

you can't do that with a database, two postgres containers pointed at the same data directory will corrupt it, so stateful services are singletons. deploy starts them once and every deploy after that walks straight past them.

your db password goes in `.env.production`, not in this file, this file gets committed. push it once and then deploy:
```bash
deploy env push .env.production
deploy
```

first deploy starts pg, runs the migration against it, then starts web. every deploy after that leaves pg exactly where it is.

you don't need dependsOn for the database, deploy brings the stateful stuff up and waits for it to be healthy before anything else starts. dependsOn is for services in the same release, like a web that needs a worker running first.

changing a stateful service is the one thing that causes downtime. if you bump postgres:17 to postgres:18 or add a volume, deploy has to recreate the container, it'll tell you before it does it. no way around that one, you can't have two postgres containers sharing a data directory.

migrations have to work with the old code for a minute, because the migration runs while the old version is still serving and the old version keeps serving until the new one is healthy. so dropping a column the old code still reads will break the site during its own deploy. add first, drop later.

deploy rollback puts the old code back, it doesn't undo a migration

### config:

everything deploy knows about, and what you can put in it.

**top level**

| key | takes | what it does |
| --- | --- | --- |
| `version` | `1` | schema version. always 1 right now, deploy refuses anything it doesn't know instead of guessing |
| `id` | 8 hex chars, `a3f19c02` | how deploy identifies your project on the server. generated for you, don't change it, a different id means a whole second copy of your project |
| `name` | any string | what shows up in deploy status and deploy list |
| `destination` | `/srv/projects` or `x.x.x.x:/srv/projects` | where it goes, ssh path syntax. a colon before the first slash means remote. you can pass `-D` instead |
| `gitStorage` | `x.x.x.x:/srv/git/shop.git` | optional. if your bare repo is on the same server you're deploying to, nothing gets uploaded, the server pulls the code out of its own repo. it'll also stop you deploying a commit you forgot to push |
| `retention` | number, default `3` | how many releases to keep on the server. whatever you set, the running one and the one before it never get pruned |
| `buildOnDestination` | `true` / `false` | build on the server instead of on your machine. same as `--build-on-destination`, but you set it once instead of remembering it every time. the flag still wins if you actually pass it |
| `notify` | object | ping a discord channel when a deploy finishes, see below |
| `services` | object | the things that run |
| `release` | object | one off jobs that run before the new code starts, migrations basically |
| `environments` | object | per environment overrides, see the multi env example above |

**per service**

| key | takes | what it does |
| --- | --- | --- |
| `image` | `postgres:17` | a published image. deploy never builds it, your server pulls it |
| `build` | `"Dockerfile"`, `{ "dockerfile": "..." }`, or an inline build | build it yourself instead. exactly one of image or build, not both |
| `stateful` | `true` / `false` | true means it owns data and gets started once. false means it gets replaced every deploy |
| `env` | `[".env.production"]` | env files. a bare name is one you pushed with deploy env push, anything with a slash in it is a file you committed with your code |
| `host` | object | put it on the internet through a cloudflare tunnel |
| `dependsOn` | `{ "worker": "healthy" }` | wait on another service in the same release. takes `healthy`, `completed`, or `started` |
| `enabled` | `true` / `false`, default `true` | `false` leaves the service out entirely. set it in an environment to drop a service that environment would otherwise inherit, like `"staging": { "services": { "worker": { "enabled": false } } }`. works on release tasks too |
| `healthcheck` | object | how deploy knows it actually came up. anything hosted needs one |
| `isolated` | `"auth"` | put it on that private network and nowhere else, see private networks below |
| `joins` | `["auth"]` | also put it on these private networks, so it can reach what is isolated there |
| anything else | whatever compose takes | `ports`, `volumes`, `environment`, `command`, `restart`, `logging`, `mem_limit`, `cap_add`, all of it goes straight through untouched |

**healthcheck**

| key | takes | what it does |
| --- | --- | --- |
| `command` | `["CMD", "curl", "-f", "http://localhost:3000/health"]` | what to run inside the container. use `CMD-SHELL` instead of `CMD` if you need a shell, like `["CMD-SHELL", "pg_isready -U postgres"]` |
| `interval` | `"5s"` | how often to check |
| `timeout` | `"3s"` | how long one check gets |
| `retries` | number | how many failures before it's considered down |
| `startPeriod` | `"2m"` | grace period on boot where failures don't count, for stuff like a minecraft server generating a world |

**host**

| key | takes | what it does |
| --- | --- | --- |
| `domain` | `shop.example.com` | the hostname you set up on the tunnel. deploy checks this actually answers after it cuts over |
| `port` | number | the container port cloudflared talks to |
| `tunnelTokenFrom` | `"WEB_TUNNEL_TOKEN"` | the *name* of the env var holding your token, never the token itself. it gets read out of your env file on the server, so it never ends up in this file or in ps |
| `tunnelEnv` | `"tunnel.env"` | optional. the env file the token is read from, pushed with `deploy env push`. only cloudflared's token comes out of it and it is never mounted into the service, so the service cannot read the token. without it, the token has to sit in one of the service's own `env` files, and the service sees it too |

three rules deploy will hold you to: anything hosted needs a healthcheck, has to be `stateful: false`, and can't publish `ports`. two copies of the same service can't both bind 3000, which is the whole reason the cutover works.

one cloudflared per host block, so two hostnames means two tunnels and two tokens. each one runs locked down: read only, no capabilities, no new privileges, and capped at 256 processes and 256 MB.

**private networks**

every service sits on one project network, so by default your api, your workers and your tunnels can all reach your database's port. a private network fences something off:

```jsonc
"auth-db":     { "image": "postgres:18", "stateful": true, "isolated": "auth" },
"auth":        { "build": "Dockerfile", "joins": ["auth"], "host": { ... } },
"auth-migrate":{ "build": "Dockerfile", "isolated": "auth" }   // a release task works too
```

- `isolated` puts a service on the private network only. nothing else can resolve its name or reach its ports, and it cannot reach the internet, since deploy creates the network as an internal docker network.
- `joins` adds the private network beside the project network, so the service reaches what is isolated there and everything else as usual.
- anything with a `dependsOn` on an isolated service has to join its network, an isolated service cannot be hosted, publish `ports` or declare endpoints, and every member has to be on the same destination. deploy check tells you which rule you broke.
- if a network with the same name already exists and is not internal, deploy refuses to use it rather than quietly losing the isolation. destroy removes private networks with the project network.

**locking a container down**

deploy passes compose keys it does not own straight through, so hardening a service is a few keys on it:

```jsonc
"read_only": true,
"tmpfs": ["/tmp"],
"cap_drop": ["ALL"],
"security_opt": ["no-new-privileges:true"],
"pids_limit": 256,
"mem_limit": "512m",
"user": "1000:1000"
```

**notify**

```jsonc
"notify": {
  // the NAME of the variable holding your webhook, never the webhook itself
  "discordWebhookFrom": "DISCORD_WEBHOOK_URL"
}
```

you don't have to write any of that yourself, `deploy dwh` does it:

```bash
deploy dwh https://discord.com/api/webhooks/...   # sets it up
deploy dwh                                        # says whether one is set
```

that writes the url to `.deploy/secrets.env` at mode 600, adds the notify block to your config, makes sure `.deploy/` is gitignored, and posts a test message so you find out right there if the url is wrong.

a webhook url is a credential, anyone who has it can post in your channel, so it goes in the config by name the same way a tunnel token does. deploy looks the name up in two places, your shell first and then `.deploy/secrets.env`.

`.deploy/` is gitignored, and deploy re-adds that line every run, so the webhook stays out of your repo. it never reaches your server either, because `git archive` only ships tracked files. the shell wins over the file, which is what makes ci work without one.

this channel is for the people using your thing, not for you, so it says nothing until there's something true to tell them. a lock somebody else holds, a dirty tree, a build that fell over, none of that reaches them. it's one message, posted once the images are built and the databases are up, and then rewritten as the release moves. release tasks and container starts are the slow part and they come after, so the message is up while people are waiting rather than arriving all at once at the end.

it starts blue:

```
staging new deployment
14:05:09  there is a new version being staged

version              affected
abc1234 -> def5678   ```
                     lecternmc.com
                     bot
                     ```
```

turns orange and gains a line when the switch starts, then green when it's done:

```
update live
14:05:09  there is a new version being staged
14:05:20  switching over to the new version now, usually without any interruption
14:05:31  the new version is up and answering
```

and red if it doesn't work out, so nobody's left sitting on a switching over line that never resolved:

```
reverting changes
14:05:09  there is a new version being staged
14:05:20  switching over to the new version now, usually without any interruption
14:05:24  we are reverting changes while we fix some issues with the deployment
```

nothing is ever taken away, so somebody who looks at the channel an hour later reads the whole release out of the one message. the title and the colour are always the state it's in now.

the `version` field reads `abc1234 -> def5678` going forward and `abc1234 <- def5678` when it reverts, so the arrow always points at whichever release you end up on. a first deploy has nothing to move away from so it just says `def5678`.

the `affected` field names hostnames rather than services, one per line, because somebody reading the channel knows the site by its domain and has never heard of the container behind it.

rollback and destroy talk to the same channel, since they change what people are looking at just as much as a deploy does. each is its own message rather than a late edit of the deploy before it:

```
orange  rolling back          we are going back to an earlier version, usually without any interruption
red     rollback failed       the earlier version could not be started, so nothing changed
orange  <project> is experiencing downtime
```

the rollback notice goes out as it happens rather than after, because the whole thing takes under a second and there's nothing useful to say twice. it only speaks again if it couldn't start the earlier version, in which case nothing changed and the message says so.

destroy says the one thing your users need, which is that it's down. whether you kept the volumes is your business and doesn't go in the channel.

if the webhook is down, your deploy still ships. a notification is a report about a deploy and never part of one.

**inline build**, if you don't want to write a dockerfile:

| key | takes | what it does |
| --- | --- | --- |
| `from` | `"node:24-slim"` | base image, the only one you actually need |
| `packages` | `["git", "curl"]` | installed with apt |
| `packageManager` | `"apt"` / `"apk"` | apt unless you say otherwise. deploy won't guess it off your base image |
| `workdir` | `"/app"` | where the rest of it runs |
| `run` | `["npm ci", "npm run build"]` | one RUN each, after your code is copied in |
| `env` | `{ "NODE_ENV": "production" }` | baked into the image |
| `expose` | number | the port it listens on |
| `start` | `"npm start"` | what runs when the container starts |

fine for something small. it's a single stage build so every source change re-runs the whole install, and a cross arch build emulates the whole way through. if that starts hurting, write a real multi stage dockerfile and point `build.dockerfile` at it.

### commands:

| command | what it does |
| --- | --- |
| `deploy` | deploy the current commit |
| `deploy -e staging` | deploy a specific environment |
| `deploy check` | validate your config and print it back, changes nothing |
| `deploy status` | what's running, per service health, what's exposed |
| `deploy list` | every project on that server, not just this one |
| `deploy releases` | what's still on the server, current one marked |
| `deploy logs web` | tail a service, `-f` to follow it |
| `deploy shell web` | a shell inside the running container |
| `deploy exec web -- npm run seed` | run one thing inside it |
| `deploy env push .env.production` | put a secrets file on the server, mode 600 |
| `deploy dwh <url>` | set the discord webhook for this project |
| `deploy dwh` | say whether a webhook is set, without printing it |
| `deploy rollback` | back to the previous release |
| `deploy rollback 9f4be0a` | back to any release still on the server |
| `deploy resolve 3f2a9c1e0b7d4a65` | clear an operation that did not finish, after checking what it changed |
| `deploy network bootstrap` | install the root network helper on each host of a managed network, with your sudo |
| `deploy network status` / `plan` / `apply` | show, preview or converge the managed network on every host |
| `deploy destroy` | remove the project, keeps your data |
| `deploy destroy --volumes` | remove the data too |

logs, shell and exec work out which stack the service is in themselves, you never have to know whether something is stateful or what commit is live.

**flags**

| flag | what it does |
| --- | --- |
| `-C, --context` | run against a different directory |
| `-D, --destination` | override where it goes |
| `-G, --git-storage` | override where the bare repo is |
| `-e, --environment` | which environment, prod by default |
| `--allow-dirty` | deploy with uncommitted changes. it still builds the commit, not your working tree |
| `--force-unlock` | clear abandoned legacy lock metadata after checking the old process has stopped; never bypass a live lock |
| `--build-on-destination` | build on the server instead of on your machine |

**exit codes**, if you're scripting it:

| code | means |
| --- | --- |
| 0 | worked |
| 1 | failed and cleaned up after itself, your old version is still serving |
| 2 | something wasn't right before it started, nothing was attempted |
| 3 | it deployed and it's live, but something after that needs you to look at it |

3 is on purpose. once traffic is on the new version, tearing it down over a failed cleanup would be causing an outage to report one

### version 2

Version 2 makes every service and release task choose a named destination. Every project command works with it, across as many destinations as the config names: `deploy` (including `--affected` and `--service`), `rollback`, `destroy`, `env push`, `resolve`, `status`, `releases`, `list`, `logs`, `shell` and `exec`, plus offline validation, planning and a migration preview. `deploy adopt` records a version 1 deployment and works on a config with one destination. Connections between services on different hosts run over a private WireGuard network that deploy sets up and keeps in step itself; see the managed-network helper below.

Limits of this release: one destination per host per environment (two destinations on one machine are refused, since their Docker names would collide), endpoints are TCP over IPv4, managed-network hosts must be Debian or Ubuntu with systemd and rootful Docker on iptables, peers connect directly with at least one reachable UDP endpoint per pair, and a stateless service that publishes a fixed port for another host is replaced in place, so it is briefly down on every deploy.

```json
{
  "version": 2,
  "id": "1234abcd",
  "name": "example",
  "destinations": {
    "app": "app-host:Projects/example",
    "data": "data-host:Projects/example"
  },
  "network": {
    "cidr": "10.77.0.0/24",
    "peers": {
      "app": {"address": "10.77.0.1", "listenPort": 51820, "keepalive": 25},
      "data": {"address": "10.77.0.2", "listenPort": 51820, "endpoint": "data.example.com:51820"}
    }
  },
  "services": {
    "api": {
      "destination": "app",
      "build": {"dockerfile": "Dockerfile"},
      "dependsOn": {"db": "healthy"},
      "connections": {
        "database": {"service": "db", "endpoint": "sql", "hostEnv": "DB_HOST", "portEnv": "DB_PORT"}
      }
    },
    "db": {
      "destination": "data",
      "build": {"dockerfile": "Dockerfile.db"},
      "stateful": true,
      "healthcheck": {"command": ["CMD", "pg_isready"]},
      "endpoints": {"sql": {"port": 5432, "hostPort": 5432, "protocol": "tcp"}}
    }
  }
}
```

- `deploy check -C <directory> -e production` validates every version 2 environment and prints the selected environment's plan. It does not require a Git commit.
- `deploy plan -C <directory> -e production` also records the full HEAD commit and the resolved working-tree configuration digest. It does not read secrets or contact destinations. The generated host and port values are the ones deploy injects.
- `deploy migrate -C <directory>` prints a version 1 conversion to stdout without writing the source. Add `-D host:Projects/example` if the old config has no destination. It preserves service options, release tasks, environments, notification settings and unknown legacy fields. Review the preview in a separate directory and run `check` there: stricter version 2 rules may require manual changes. Do not redirect into the source file, since the shell would truncate it before migration reads it.

There is no root `destination` in version 2. Services and tasks inherit an explicit destination from their base definition unless an environment replaces it. Names use lowercase slugs of at most 32 characters. Endpoints currently support TCP over IPv4. A remote connection needs a stable provider `hostPort`, both declared peers and at least one configured UDP endpoint. Credentials remain an application concern.

`dependsOn` controls readiness waves; `connections` controls communication. Bidirectional connections do not create a readiness cycle. `completed` requires a release task, and `healthy` requires an enabled health check. A release task must wait for its connection providers to be healthy. Service/task names cannot overlap.

Endpoint and connection overrides replace whole named entries; an empty map clears inherited entries. An empty `dependsOn` clears inherited dependencies. An environment's network replaces the entire network definition. Non-production environments must be declared explicitly. Environments sharing a host need distinct network subnets and listen ports. Raw Compose topology overrides such as `network_mode`, `networks`, `depends_on`, privileged capabilities and `container_name` are refused; use `isolated` and `joins` for private networks. Use `env` for secret-file references; raw `env_file` is refused. Endpoint declarations cannot be combined with raw `ports` on the same service.

A single-host plan, or several hosts without cross-host connections, needs only Docker networking. An unchanged network will eventually be reconciled without restarts, but this offline plan cannot determine actual changes. Host identity, SSH aliases, canonical paths, live releases, runtime environment collisions and firewall/route availability remain unverified. Same literal SSH hostnames are configuration hints, not verified machine identities. Retained releases may still need an existing tunnel. No interface, key, route, firewall rule or deployment directory is created by these commands.

For a read-only live check, run `deploy plan --observe -C <directory> -e production`. This inspects all named destinations over SSH, or locally for local paths. It resolves symlinks and missing path suffixes without creating directories, compares Linux machine/boot identities and the local Docker daemon identity, and replans connections using the observed host groups. Two SSH aliases reaching the same host do not need a tunnel between them. Conflicting peer settings or private ports on that host are refused. Contradictory machine/daemon identities are refused rather than guessed.

Observation requires Linux, `realpath`, a machine identity in `/etc/machine-id` or `/var/lib/dbus/machine-id`, and access to rootful Docker's `/var/run/docker.sock`. It ignores Docker context/host overrides. SSH keys must already be trusted: it does not add known-host entries, create multiplexing sockets, enable forwarding or request sudo. Your existing SSH configuration and any configured proxy remain trusted transport inputs. Each destination has a 15-second timeout; a failure stops the plan instead of treating the destination as empty. The output includes a hashed host identity, resolved path, user ID, architecture and Docker version; raw machine/boot/daemon IDs are omitted.

The observed plan also reads what each destination has recorded, with the same SSH options and a 1 MiB limit per file. `previousRelease` is the live commit of each service, or the last recorded run and its outcome for a release task. `recordedState` says where that came from: `version 2` per-service state, `version 1, adopted in memory` for an existing single-commit deployment, or `none`. Version 1 state is translated without writing anything, and a version 1 deployment never recorded release task runs. Services that are recorded at a destination but no longer configured there are listed in `recordedOnly`, so a removed or moved service is not mistaken for a new one. An unfinished operation is shown on its destination with the `deploy resolve` command it needs. State recorded for another project, environment or destination stops the plan.

Version 2 state lives in `services.json` beside `state.json`. It records each service's current and previous release, retained releases and outcome, every release task run with its operation id, commit, config digest and outcome, and a generation that increases on every write. Release tasks with an indeterminate outcome are never repeated automatically. Its resource naming is `legacy`, meaning existing shared project, network and volume names are kept. Every recorded release also names its compose project and file, relative to the project root, so cutover and rollback can address one service. A release adopted from version 1 points at its commit stack (`deploy-<id>-<commit>`, `releases/<commit>/docker-compose.yml`); a stateful service points at the shared stack; a new release of one stateless service gets its own project `deploy-<id>_<service>_<commit>` and file `releases/<commit>/<service>.compose.yml`. Underscores never appear in runtime ids, version 2 service names or commits, so these names cannot collide with version 1 stacks or another environment. Any other project or file in the state is refused. Each retained release also records, under `paths`, what it needed from the managed network (`connect <address>:<port>` for a provider on another host, `publish <address>:<port>` for a port another host reaches). A connection to a provider on the same host is rendered as the provider's service name and container port in the consumer's environment; one to a provider on another host as the provider's private address and its `hostPort`. Once `services.json` exists, version 1 deploy, rollback, destroy and env push refuse to change that deployment.

Version 2 commands that change several destinations will lock all of them before changing any. Locks are always taken in the same order, by observed host identity and then canonical path, never by destination name or SSH alias. Two names that reach the same directory on the same host are refused, since one root would have two owners. If any lock is busy, the locks already taken are released and nothing changes. Once every lock is held, each destination is observed again. Its host, path, boot and Docker daemon must match the plan, and its recorded state generation must be unchanged; otherwise the command stops and asks for a new plan. One operation record, under one id, is then written to every destination before the first change. These locks share the same lock files as single-destination commands, so a version 1 command through any alias or symlinked path waits its turn. `deploy`, `rollback`, `env push`, `destroy` and `deploy network apply` take them.

This is an observation of the current host, not a persistent ownership claim. Privileges, systemd, firewall rules, routes, secrets and application reachability remain unchecked, and host facts must be rechecked under locks before any future mutation.

#### deploying with version 2

`deploy` with a version 2 config observes every destination, refuses two on the same host, checks that the managed network's hosts run this release's helper, locks every destination in the global order, checks each is still what was observed, records one operation on all of them and then:

1. Writes each destination's `services.json` before changing anything, so the deployment is owned per service from the first step and version 1 deploy refuses it from then on.
2. Places the release on each destination and builds only what runs there, for that destination's architecture. A project that only runs published images builds nothing and needs no Docker on the machine running deploy.
3. Applies the managed network, the same reconciliation as `deploy network apply`, under the same locks and operation. It does nothing when no connection crosses hosts.
4. Brings up each destination's shared stack with its stateful services and tunnels under their existing names.
5. Walks the readiness waves from `dependsOn` across every destination, so a graph may go from one host to another and back. Release tasks in a wave run first, one at a time, on their own destination; each run is recorded as `indeterminate` before it starts and as `completed` or `failed` after. Stateless services in a wave start together on every host, each in its own compose project, and the wave waits until all of them are healthy. A stateless service that publishes a fixed port for another host cannot run beside its live release, so it is replaced in place: the live one stops, the candidate must become healthy, and the live one is started again if it does not. A failed task or an unhealthy candidate stops the deploy, removes every candidate it started, and leaves every other service on its previous release.
6. Reaches each cross-host provider from each candidate consumer's project network before anything is switched. A partition or a firewall that blocks the path stops the deploy there.
7. Switches each service on its own: the candidate already answers on the project network, the live container is stopped, a hosted service's public hostname is verified (the live one is started again if it does not answer), and the stopped container is removed. Its compose file stays for rollback. If one switch fails, the services already switched stay switched, the rest keep their previous releases, and each destination's state records exactly that.
8. Removes a stateless service from a destination the config moved it away from, now that it is live where it moved to. Moving a stateful service is refused before anything changes, since its data would stay behind. A service dropped from the config keeps running and stays recorded until `deploy destroy` removes it.
9. Records each switched service's current, previous and retained releases, then prunes per service. A release directory and its images are removed only once no service still refers to that commit.
10. Tells each managed-network host which containers to start once its network is up after a reboot or a Docker restart (see `workloads` below).

`deploy --service api,worker` deploys only those services and release tasks. It refuses a name the environment does not have, and a selected service that waits on something neither selected nor already live; stateful services always come up with their shared stack.

A release task whose last run is `indeterminate` blocks the next deploy. The interrupted operation is still recorded too, so `deploy resolve <id>` after inspecting what the task did clears it and marks that run `resolved`; it is never repeated automatically. Release tasks run on every deploy, as in version 1, so they must be safe to run again.

A destination that holds a version 1 deployment is refused until it is adopted. `deploy adopt -C <directory>` previews what it would record: each service's live and previous commit stack, the shared stack for stateful services, and services that the live stack runs but the config no longer names, which are kept on record and left running. `deploy adopt --write` records it under the lock and an operation record. Nothing is restarted, renamed or moved; the next version 2 deploy moves each configured service out of its commit stack one at a time, and stateful volumes keep their names and contents.

`--affected` compares each service with the commit it is actually running, so services that move independently are each judged against their own release, and a release task against its last completed run. `--force-unlock` is refused with version 2, and so is `--destination` except by `destroy`, which takes a destination name.

`deploy rollback` returns every stateless service to its own previous release, or `--service api,worker` only those. `deploy rollback <commit>` moves each service that ran that commit, and with `--service` the named ones must have it. Each service is rolled back on the destination that records it, under the locks of every destination. The retained release is started from its compose file, whether its own project or a version 1 commit stack, must become healthy, and then takes over exactly as a deploy candidate does; one that will not start is removed and nothing serving is touched. A fixed-port service is rolled back in place, the way deploy replaces one. A release that needed a network path the current config no longer provides is refused: rolling back never touches the network, so it cannot bring revoked access back, and a deploy is the way forward. Stateful services and data are never rolled back. Rolling back again returns each service to where it was.

`deploy destroy` finds every compose project deploy created for this environment by name on the Docker engine, including candidates and task runs an interrupted deploy left behind, and never another environment's. It keeps the typed confirmation, re-plans under the lock and refuses if anything changed while you were confirming, and records the operation. It removes the projects, images, Docker network and release directories on every destination, then the managed network from each host that has one. Volumes and env files stay unless `--volumes` is given, and so does an empty ownership record, so the next deploy or destroy there knows whose they are. `deploy destroy -D <destination>` destroys one destination by its config name; its host keeps the managed network while the rest of the config still links it.

The everyday commands follow each service's recorded release: `status` shows every service on every destination with its release, previous release and container status, including services the config no longer names; `releases` lists retained releases per service and each release task's last run, per destination; `logs`, `shell` and `exec` go to the destination the config places the service on (or, for one it dropped, the destination that still records it) and reach it in whichever project serves it, its own release, a version 1 commit stack or the shared stack; `list` shows every project on each destination, with the live commits of a per-service deployment. `env push <file>` sends the file only to the destinations whose services or release tasks list it in `env`, refuses a file nothing lists, takes those destinations' locks and records the operation like a deploy; on a destination that was never deployed it claims the root per service. `resolve <id>` clears the operation on every destination that recorded it. Commands that read state refuse a version 1 deployment that was not adopted, and state recorded for another project, environment or destination.

### managed-network helper

A managed network needs root on each of its hosts, and deploy gets it through one small helper rather than broad sudo. `deploy network bootstrap -C <directory> -e production` lists the hosts of that environment's network, shows the helper's path, version and sha256, the sudoers rule and the package step, and asks you to type `bootstrap`. On each host it then runs your own `sudo` once, with a password prompt, to install `/usr/local/libexec/deploy-network` owned by root, check the installed copy against the checksum, install `wireguard-tools` with apt if `wg` is missing, and write `/etc/sudoers.d/deploy-network` after `visudo` accepts it. That rule lets the SSH user run only `deploy-network version`, `observe`, `enroll`, `apply`, `remove` and `workloads` without a password, and nothing else as root. A host that already runs this release's helper is skipped, so a second bootstrap asks nothing. The helper is embedded in the deploy binary, so a deploy release and the helper it expects never come from different builds.

The helper reads one network as plain `field value` lines on stdin and refuses anything else: unknown or repeated fields, stray whitespace, invalid keys, addresses outside the subnet, public subnets, or a runtime id that does not match its environment. It renders the WireGuard config and firewall rules itself, so nothing deploy sends is written or run verbatim. Everything it creates is named after the runtime id: interface `dw<12 hex>`, chains `DEPLOY-<12 HEX>-IN/-FWD/-OUT`, `wg-quick@<interface>`, the boot unit `deploy-network-<interface>.service`, `/etc/wireguard/<interface>.conf` and `.key`, and `/var/lib/deploy-network/<interface>/`. Ownership is proven by the owner manifest there, never by a name. `enroll` refuses an interface, key, unit or chain that exists without one, a UDP port already in use and a subnet that overlaps an existing route. Private keys are generated on the host and never printed; only public keys leave it. `apply` changes only what differs: a peer change syncs in place, only a changed interface address or port restarts WireGuard, flushed chains, missing jumps and lost routes are repaired, and an unchanged network reports `unchanged` with nothing written. After a reboot or a Docker restart the boot unit puts the owned chains, peer routes and jumps back, waiting up to 90 seconds for Docker's `DOCKER-USER` chain. `remove` deletes everything the runtime id owns and nothing else. If `apply` fails part way, it puts back the files and state it had before and says so; one that changed something keeps the files it replaced in `previous/`. Supported hosts are Debian or Ubuntu with systemd and rootful Docker on iptables; anything else is refused with the reason.

A container that publishes onto the managed network, or reaches a provider on another host, must not start before the private address, routes and firewall exist: Docker cannot bind a missing address, and a consumer with no route would send private traffic over the default one. Deploy gives such services `restart: on-failure`, which Docker leaves stopped at boot and on a daemon restart but still restarts after a crash, and refuses a `restart` of their own. After every deploy and rollback it records the live ones per host with the helper's `workloads` verb, in readiness order, and the boot unit starts them once the network is back. Only container names of this runtime's own compose projects are accepted.

Three commands drive the helper across every host of an environment's network. Each first observes the destinations, so two SSH aliases of one machine count as one host, then asks every host's helper through `sudo -n` with the same strict SSH options as `deploy plan --observe`. A missing, outdated or disallowed helper stops them with the bootstrap instruction.

- `deploy network status` prints each host's interface, public key, WireGuard and boot unit state, peers, routes and firewall, and whether it matches the desired network. Private keys are never read.
- `deploy network plan` prints only what apply would do on each host: `enroll`, `unchanged`, or the reasons it differs, such as a changed config, drifted chains, a missing jump or route, or a stopped interface.
- `deploy network apply` takes the lock of every destination on the network in the usual order, checks each still holds this environment, and records one operation. It then observes again, enrolls hosts that have no identity yet, exchanges public keys, and applies each host in turn. A host that already matches changes nothing. Each destination keeps `network.json` beside its state, with the applied input's digest and a generation that goes up only when something changed there. A failure stops before the next host; apply again once it is fixed, since every step is safe to repeat. A host whose private key was lost is refused rather than given a new identity silently.

### managed-network experiments

`test/network` probes the proposed WireGuard, firewall, service supervision and remote-operation locking mechanisms, then installs the network helper with the real deploy binary, exercises it, and drives `deploy network status`, `plan` and `apply` across both guests. The `acceptance` phase then runs production and staging networks side by side, refuses a second project's used port and overlapping subnet, reboots a guest, reloads UFW, restarts Docker, removes a route, presents an unsupported OS and a missing `DOCKER-USER` chain, rebuilds a peer's identity, fails an apply on the second host and checks that no private key reached any deploy output. `workloads` checks boot ordering through Docker restarts and reboots of both guests. `deploy` runs real version 2 deploys across both guests: a fresh deploy that provisions the network, a no-op redeploy, a failed candidate on the second host, env push routing, status, releases and exec, `--service`, rollback, a reboot, a refused stateful move, a moved service, and scoped and full destroy. `orchestration` covers a readiness graph that returns to a host, a fixed-port provider replaced in place, a network partition caught before switching, a release task cut off mid-run and an interrupted cutover, both recovered by a second operator from its own clone and key.

On Linux amd64 with local rootful Docker, accessible `/dev/kvm`, Go, Node 24, about 3 GiB available memory and 6 GiB free disk, run:

```sh
DEPLOY_NETWORK_OUTPUT=/absolute/path/outside/the/checkout/network-results bash test/network/run.sh
```

The fixture downloads pinned Debian 12, Docker 29.1.3 and Docker Compose 5.5.1, creates two disposable VMs inside a tools container, and tests real WireGuard traffic, NAT, UFW, no-op updates, reboots, rollback and disconnected SSH lock ownership. Guest packages require internet access. Only KVM is passed through; no host Docker socket, host network or privileged container is used. Firewall and systemd changes occur inside the VMs. The `network lab` workflow runs every phase on GitHub's hosted runners for pull requests that touch the network or the lab, or by hand.

`DEPLOY_NETWORK_PHASES` runs a subset, for example `setup network helper reconcile`; `setup` is always needed first, `reconcile` needs `helper`, and `acceptance` needs both.

Logs remain in the output directory. The runner removes its container, image tag, VM disks and disposable keys on success or failure; Docker build cache may remain. Do not run concurrently against the same output directory. This opt-in suite is separate from normal Go CI. It covers Debian amd64 with Docker's iptables backend; other platforms, power-loss durability and the future CLI integration still require acceptance tests.

