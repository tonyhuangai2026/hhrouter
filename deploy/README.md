# CloudFormation deployment

`cloudformation.yml` stands up the full Agent Router Platform on AWS in one stack:

- a new **VPC** (2 public subnets across AZs, IGW, routing) — fully self-contained;
- a single **EC2 instance** (Ubuntu 24.04) that, on boot, installs Docker, `git clone`s
  this repo, writes `.env`, and runs `docker compose up -d --build` (postgres + redis +
  backend + frontend);
- an internet-facing **Application Load Balancer** that forwards HTTP `:80` → the
  instance's frontend nginx `:8080`, health-checking `/api/ping`.

The stack output **`AlbUrl`** is the public address — open it in a browser; the first
account you register becomes admin.

## Deploy

```bash
aws cloudformation deploy \
  --region us-east-1 \
  --stack-name agent-router \
  --template-file deploy/cloudformation.yml \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides \
      RepoUrl=https://github.com/<you>/agent-router-platform.git \
      RepoBranch=main \
      JwtSecret="$(openssl rand -hex 32)" \
      SecretKey="$(openssl rand -hex 32)" \
      PostgresPassword="$(openssl rand -hex 16)" \
      RedisPassword="$(openssl rand -hex 16)"
```

Then read the address back:

```bash
aws cloudformation describe-stacks --region us-east-1 --stack-name agent-router \
  --query "Stacks[0].Outputs[?OutputKey=='AlbUrl'].OutputValue" --output text
```

Stack creation waits (up to 20 min, `CreationPolicy`) for the instance to finish the
Docker build and signal success, so when `deploy` returns the app is already serving.

## Key parameters

| Parameter | Required | Default | Notes |
|-----------|----------|---------|-------|
| `RepoUrl` | **yes** | — | Git URL the instance clones. Must be reachable from the instance (a public HTTPS git URL works with no extra setup). |
| `RepoBranch` | no | `main` | Branch/ref to check out. |
| `JwtSecret` | **yes** | — | Signs admin JWTs. Use a long random string. |
| `SecretKey` | **yes** | — | Encrypts stored channel keys (AES-256-GCM). |
| `PostgresPassword` | no | `postgres` | Set a strong value in production. |
| `RedisPassword` | no | empty | Recommended. |
| `InstanceType` | no | `t3.medium` | Build needs RAM; `t3.small` is too small to build the frontend reliably. |
| `AdminUsername` / `AdminPassword` | no | empty | If both set, an admin is seeded on first start (otherwise register via the UI). |
| `SshAllowedCidr` + `KeyName` | no | empty | Set both to enable SSH `:22` from that CIDR for debugging. |

## Updating an existing deployment

After you push new code to the repo, there are two ways to roll it out.

### Option A — CloudFormation redeploy (clean, replaces the instance)

Bump the `DeployVersion` parameter and run `update-stack`. Changing it alters the
instance's UserData, so CloudFormation **replaces** the instance: a fresh box
boots, re-clones the latest `RepoBranch`, and rebuilds the stack.

```bash
aws cloudformation update-stack \
  --region us-east-1 \
  --stack-name <stack> \
  --use-previous-template \
  --parameters \
      ParameterKey=DeployVersion,ParameterValue=2 \
      ParameterKey=RepoUrl,UsePreviousValue=true \
      ParameterKey=RepoBranch,UsePreviousValue=true \
      ParameterKey=InstanceType,UsePreviousValue=true \
      ParameterKey=JwtSecret,UsePreviousValue=true \
      ParameterKey=SecretKey,UsePreviousValue=true \
      ParameterKey=PostgresPassword,UsePreviousValue=true \
      ParameterKey=RedisPassword,UsePreviousValue=true
```

> Pass the **same** template (`--use-previous-template`) and increment
> `DeployVersion` each time (2, 3, 4…). The ALB DNS name is unchanged, so the
> public URL stays the same.
>
> ⚠️ **Data loss:** replacing the instance discards its EBS volume, so the
> Postgres/Redis data on the box is lost (users, channels, keys, logs). This is
> fine while still configuring, but for a live deployment with real data prefer
> Option B, or move the database to RDS first.

### Option B — in-place update (keeps data + URL, needs SSH)

Deploy SSH access (`SshAllowedCidr` + `KeyName`), then on the box:

```bash
ssh ubuntu@<instance-ip>
cd /opt/agent-router
git pull
docker compose --env-file .env up -d --build   # rebuilds only changed images
```

This rebuilds the containers in place — Postgres/Redis volumes are untouched, so
all data and the ALB URL survive.

## Notes & limitations

- **HTTP only.** Per the chosen scope this exposes plain HTTP on the ALB DNS name. For a
  real domain + HTTPS, add an ACM cert and a `:443` listener (the existing prod box uses
  Caddy for that instead — see the project's prod-deployment notes).
- **Single instance, build-on-box.** The instance both builds and runs the images
  (matches the README quickstart). It is *not* an autoscaling/HA setup — the ALB here is
  for the stable public address, not redundancy. State (postgres/redis) lives on the
  instance's EBS volume and is lost if the instance is replaced.
- **Bootstrap log:** if the app doesn't come up, SSH in (enable SSH params) and read
  `/var/log/arp-bootstrap.log`.
- **Teardown:** `aws cloudformation delete-stack --stack-name agent-router`. The VPC, ALB,
  instance, and EBS volume are all deleted with the stack.
```
