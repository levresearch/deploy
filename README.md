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
| `healthcheck` | object | how deploy knows it actually came up. anything hosted needs one |
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

three rules deploy will hold you to: anything hosted needs a healthcheck, has to be `stateful: false`, and can't publish `ports`. two copies of the same service can't both bind 3000, which is the whole reason the cutover works.

one cloudflared per host block, so two hostnames means two tunnels and two tokens.

**notify**

```jsonc
"notify": {
  // the NAME of the variable holding your webhook, never the webhook itself
  "discordWebhookFrom": "DISCORD_WEBHOOK_URL"
}
```

a webhook url is a credential, anyone who has it can post in your channel, so it goes in the config by name the same way a tunnel token does. deploy looks the name up in two places, your shell first and then `.deploy/secrets.env`:

```bash
mkdir -p .deploy
umask 077 && echo 'DISCORD_WEBHOOK_URL=https://discord.com/api/webhooks/...' >> .deploy/secrets.env
```

`.deploy/` is gitignored, and deploy re-adds that line every run, so the webhook stays out of your repo. it never reaches your server either, because `git archive` only ships tracked files. the shell wins over the file, which is what makes ci work without one.

you get one message at the end of every deploy, green for a success, red for a failure with the error, amber for exit code 3 where it went live but something after that needs you. it says which services moved and calls out any stateful service that got recreated.

rollback and destroy notify too, since they change what's serving just as much as a deploy does. a rollback says where it came from as well as where it landed, and a destroy says whether it took your volumes with it.

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
| `deploy rollback` | back to the previous release |
| `deploy rollback 9f4be0a` | back to any release still on the server |
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
| `--force-unlock` | break a lock left behind by a deploy that died |
| `--build-on-destination` | build on the server instead of on your machine |

**exit codes**, if you're scripting it:

| code | means |
| --- | --- |
| 0 | worked |
| 1 | failed and cleaned up after itself, your old version is still serving |
| 2 | something wasn't right before it started, nothing was attempted |
| 3 | it deployed and it's live, but something after that needs you to look at it |

3 is on purpose. once traffic is on the new version, tearing it down over a failed cleanup would be causing an outage to report one

