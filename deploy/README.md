# CloudFormation deployment

Two templates, two availability postures. Pick one:

| Template | Topology | Use when |
|----------|----------|----------|
| `cloudformation.yml` | 1 EC2 running docker-compose, new VPC created for you | PoC / demo. Cheapest (~$16/mo). **Losing the instance loses the data** — Postgres lives on its EBS volume. |
| `cloudformation-ecs.yml` | ECS Fargate, 2 tasks across 2 AZs, RDS + ElastiCache | Anything real. ~$70/mo. A task or an AZ can die without an outage; data outlives the compute. |

Both put an ALB in front, so the public URL is stable either way.

- [Option 1 — single EC2](#option-1--single-ec2-cloudformationyml)
- [Option 2 — ECS Fargate](#option-2--ecs-fargate-cloudformation-ecsyml)

---

# Option 1 — single EC2 (`cloudformation.yml`)

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

---

# Option 2 — ECS Fargate (`cloudformation-ecs.yml`)

Removes the single-node failure mode: the application runs as **2 Fargate tasks
across 2 AZs** behind an ALB, and the state that cannot live in a container moves
to managed services.

```
        Internet
           │
    ┌──────▼──────┐   ACM cert (optional) terminates TLS here
    │     ALB     │   idle_timeout 300s ← required for SSE
    └──────┬──────┘
    ┌──────┴───────────────┐  private subnets, 2 AZs
    │  ECS Fargate task    │  frontend nginx :80 ─┐ same network namespace,
    │  (×2, one per AZ)    │  backend Go   :3000 ─┘ so nginx talks to 127.0.0.1
    └──────┬───────────┬───┘
           │           └──────────► NAT ──► Bedrock / gateways / SageMaker probe
    ┌──────▼──────┐  ┌─────────────┐
    │ RDS Postgres│  │ ElastiCache │
    │ (the data)  │  │ Redis (cache)│
    └─────────────┘  └─────────────┘
```

**Why this migration was cheap:** the application is already stateless — it writes
nothing to local disk, keeps no in-process cache, and signs JWTs with HS256 rather
than holding server-side sessions, so tasks share login state for free. The only
state that had to move was PostgreSQL. Redis is *not* authoritative: quota
counters lazily reseed from the DB's `used_quota` columns and the channel model
list is a 10-minute cache, so losing that node costs seconds of accuracy, not data.

## Prerequisites: the VPC

This template **does not create a VPC** — you pass one in. (Accounts routinely sit
at the 5-VPC default limit, and blocking a deployment on a quota increase is the
wrong tradeoff.) Bring your own, whether that is an existing VPC or one you create
fresh for this purpose.

What the VPC must provide:

| Requirement | Why |
|-------------|-----|
| **Two public subnets in different AZs** | They host the ALB (which requires ≥2 AZs) and the NAT gateway. Pass both to `PublicSubnetIds`. |
| Those subnets have a **default route to an internet gateway** | Otherwise the ALB is unreachable and the NAT gateway cannot egress. |
| **Free CIDR space for two /20 private subnets** | The template creates them (`PrivateSubnetACidr` / `PrivateSubnetBCidr`) for the tasks, RDS and Redis. Defaults assume a `10.20.0.0/16` VPC — override if yours differs. |
| `enableDnsSupport` + `enableDnsHostnames` | The tasks resolve the RDS and ElastiCache endpoints by DNS name. |

### Creating a fresh VPC for this stack

If the customer wants a dedicated VPC, this is the layout to create. **Check the
VPC quota first** — the default limit is 5 per region, and hitting it is the most
common reason this step fails:

```bash
aws ec2 describe-vpcs --query 'length(Vpcs)' --output text   # in use
aws service-quotas get-service-quota --service-code vpc \
  --quota-code L-F678F1CE --query 'Quota.Value' --output text # limit
```

Raise it via Service Quotas if needed (an internet gateway is limited per VPC
count too, so both hit the wall together). Then:

```bash
# 1. VPC with DNS enabled
VPC=$(aws ec2 create-vpc --cidr-block 10.20.0.0/16 \
        --query Vpc.VpcId --output text)
aws ec2 modify-vpc-attribute --vpc-id $VPC --enable-dns-support
aws ec2 modify-vpc-attribute --vpc-id $VPC --enable-dns-hostnames

# 2. Internet gateway
IGW=$(aws ec2 create-internet-gateway --query InternetGateway.InternetGatewayId --output text)
aws ec2 attach-internet-gateway --vpc-id $VPC --internet-gateway-id $IGW

# 3. Two PUBLIC subnets in different AZs
AZ1=$(aws ec2 describe-availability-zones --query 'AvailabilityZones[0].ZoneName' --output text)
AZ2=$(aws ec2 describe-availability-zones --query 'AvailabilityZones[1].ZoneName' --output text)
SUB1=$(aws ec2 create-subnet --vpc-id $VPC --cidr-block 10.20.1.0/24 \
        --availability-zone $AZ1 --query Subnet.SubnetId --output text)
SUB2=$(aws ec2 create-subnet --vpc-id $VPC --cidr-block 10.20.2.0/24 \
        --availability-zone $AZ2 --query Subnet.SubnetId --output text)
aws ec2 modify-subnet-attribute --subnet-id $SUB1 --map-public-ip-on-launch
aws ec2 modify-subnet-attribute --subnet-id $SUB2 --map-public-ip-on-launch

# 4. Route table with a default route to the IGW, associated to both subnets
RTB=$(aws ec2 create-route-table --vpc-id $VPC --query RouteTable.RouteTableId --output text)
aws ec2 create-route --route-table-id $RTB --destination-cidr-block 0.0.0.0/0 --gateway-id $IGW
aws ec2 associate-route-table --route-table-id $RTB --subnet-id $SUB1
aws ec2 associate-route-table --route-table-id $RTB --subnet-id $SUB2

echo "VpcId=$VPC  PublicSubnetIds=$SUB1,$SUB2"
```

The `10.20.1.0/24` + `10.20.2.0/24` public subnets leave `10.20.32.0/20` and
`10.20.48.0/20` free, which is exactly what the private-subnet defaults expect —
so with this layout you do not need to override the CIDR parameters.

> Deleting the stack does **not** delete a VPC you created yourself. Tear it down
> separately (and note the stack's NAT gateway + its EIP do go away with the stack).

## Build and push the images

Fargate pulls images from a registry, so unlike Option 1 there is no build-on-box
step. Build once and push to ECR:

```bash
REGION=us-east-1
ACCT=$(aws sts get-caller-identity --query Account --output text)
REG=$ACCT.dkr.ecr.$REGION.amazonaws.com
TAG=$(git rev-parse --short HEAD)      # immutable tag, so a rollout is reproducible

for r in agent-router-platform-backend agent-router-platform-frontend; do
  aws ecr create-repository --repository-name $r --region $REGION \
    --image-scanning-configuration scanOnPush=true 2>/dev/null || true
done

aws ecr get-login-password --region $REGION | docker login --username AWS --password-stdin $REG
docker build -t $REG/agent-router-platform-backend:$TAG  ./server
docker build -t $REG/agent-router-platform-frontend:$TAG ./web
docker push $REG/agent-router-platform-backend:$TAG
docker push $REG/agent-router-platform-frontend:$TAG
```

## Deploy

```bash
aws cloudformation deploy \
  --region us-east-1 \
  --stack-name agent-router-ecs \
  --template-file deploy/cloudformation-ecs.yml \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides \
      VpcId=vpc-xxxxxxxx \
      PublicSubnetIds=subnet-aaaa\\,subnet-bbbb \
      BackendImage=$REG/agent-router-platform-backend:$TAG \
      FrontendImage=$REG/agent-router-platform-frontend:$TAG \
      JwtSecret="$(openssl rand -hex 32)" \
      SecretKey="$(openssl rand -hex 32)" \
      PostgresPassword="$(openssl rand -hex 16)" \
      AdminUsername=admin \
      AdminPassword="$(openssl rand -hex 12)"
```

> `PublicSubnetIds` is a CloudFormation *list*, so the comma must survive the shell —
> escape it (`\\,`) or quote the whole `key=value`.

Read the URL back:

```bash
aws cloudformation describe-stacks --region us-east-1 --stack-name agent-router-ecs \
  --query "Stacks[0].Outputs[?OutputKey=='Url'].OutputValue" --output text
```

## Key parameters

| Parameter | Required | Default | Notes |
|-----------|----------|---------|-------|
| `VpcId` | **yes** | — | Existing VPC (see prerequisites above). |
| `PublicSubnetIds` | **yes** | — | Exactly two public subnets in different AZs. |
| `BackendImage` / `FrontendImage` | **yes** | — | Full ECR URIs. Prefer an immutable tag over `:latest`. |
| `JwtSecret` | **yes** | — | Signs admin JWTs (HS256). |
| `SecretKey` | **yes** | — | AES-GCM key for stored channel credentials. **When migrating an existing database this must be the old value**, or every stored channel key becomes undecryptable. |
| `PostgresPassword` | **yes** | — | RDS master password. RDS forbids `/`, `@`, `"` and spaces. |
| `PrivateSubnetACidr` / `BCidr` | no | `10.20.32.0/20`, `10.20.48.0/20` | Must be free inside the VPC's range. |
| `DesiredCount` | no | `2` | Keep ≥2 — with one task there is nothing to fail over to. |
| `CertificateArn` | no | empty | ACM cert. When set, the ALB serves HTTPS `:443` and redirects `:80`. When blank, plain HTTP only (fine for the `*.elb.amazonaws.com` smoke test). |
| `AlbIdleTimeout` | no | `300` | Seconds. Must exceed the longest streaming response — see the SSE note below. |
| `DbMultiAZ` | no | `false` | `true` adds automatic DB failover (roughly doubles the RDS cost). |
| `DbInstanceClass` / `CacheNodeType` | no | `db.t4g.micro` / `cache.t4g.micro` | Adequate for this workload's config + log traffic. |

## Rolling out new code

Build and push a new tag, then point the service at it. ECS performs a rolling
replacement (`MinimumHealthyPercent: 100`, so capacity never dips), and the
deployment circuit breaker rolls back automatically if the new tasks fail health
checks.

```bash
aws cloudformation deploy \
  --region us-east-1 --stack-name agent-router-ecs \
  --template-file deploy/cloudformation-ecs.yml \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides \
      BackendImage=$REG/agent-router-platform-backend:$NEWTAG \
      FrontendImage=$REG/agent-router-platform-frontend:$NEWTAG
      # ...other params keep their previous values
```

Unlike Option 1, a rollout does **not** touch the data: Postgres is in RDS.

## Custom domain + HTTPS

1. Request an ACM certificate for the domain **in the same region as the ALB**.
2. Point a Route53 ALIAS (or a CNAME) at the `AlbDnsName` output; the ALIAS target
   hosted-zone id is the `AlbHostedZoneId` output.
3. Update the stack with `CertificateArn=arn:aws:acm:...`. The template then
   creates the `:443` listener and turns `:80` into a redirect.

## Migrating data from a single-EC2 deployment

```bash
# on the old box
docker compose exec -T postgres pg_dump -U postgres agent_router | gzip > arp.sql.gz

# from anywhere that can reach RDS (e.g. a bastion in the same VPC)
gunzip -c arp.sql.gz | psql "host=<DbEndpoint> user=postgres dbname=agent_router sslmode=require"
```

Deploy the ECS stack with the **same `SecretKey`** as the old deployment first —
channel credentials are AES-GCM encrypted with it, and a new key makes them
unreadable. Redis needs no migration (it rebuilds itself from the database).

## Notes & limitations

- **SSE and the ALB idle timeout.** `/v1/*` responses stream; to a load balancer a
  model that is still thinking looks like an idle connection. `AlbIdleTimeout`
  defaults to 300s for that reason — the AWS default of 60s truncates long answers
  mid-stream. nginx already disables proxy buffering on `/v1/`.
- **One network namespace per task.** Under `awsvpc` the two containers share a
  network stack, so nginx reaches the backend at `127.0.0.1:3000`, not at a
  `backend` service name. The image takes `BACKEND_ORIGIN` so the same build works
  under docker-compose (`backend:3000`) and Fargate.
- **One NAT gateway.** It lives in the first public subnet, so an outage of *that*
  AZ costs the other AZ's tasks their egress. A NAT per AZ removes this at roughly
  +$32/month; the ALB and the data tier are already multi-AZ regardless.
- **The task role has no AWS permissions on purpose.** The relay authenticates to
  Bedrock with per-channel bearer keys read from the database, so it needs no
  SigV4 and no IAM — a compromised container cannot reach the AWS control plane.
- **RDS is retained on stack deletion** (`DeletionPolicy: Snapshot`): deleting the
  stack leaves a final snapshot rather than destroying the data. Delete the
  snapshot separately once you are sure.
- **Container logs** go to the CloudWatch log group named by the `LogGroupName`
  output (`/ecs/<stack>`), with `backend`/`frontend` stream prefixes.
- **The routing probe is not part of this stack.** Its SageMaker endpoint is
  configured at runtime in the admin UI, so it can be repointed without a
  redeploy — which also means a dead endpoint is recovered by recreating it and
  updating the setting, not by touching this stack.
