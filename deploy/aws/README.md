# AWS deployment (CDK)

The stack: VPC, Fargate ingest behind an HTTPS-only ALB, the console behind its own HTTPS ALB (Cognito sign-in), a private API reachable only from the console (`api.bindle.internal`), Kinesis stream, Fargate processor and alerts services, OpenSearch Service domain, RDS PostgreSQL (control plane), Cognito user pool, S3 payload bucket, Secrets Manager secrets and a one-off setup task. **These resources cost money once deployed.**

```bash
cd deploy/aws && npm install
npx cdk synth -c insecureDevCidr=203.0.113.0/24     # check only (or `make synth` from the repo root)
npx cdk deploy -c certificateArn=arn:aws:acm:...    # cert for ingest.bindle.ai
```

Synth fails without a certificate: ingest carries API keys and agent payloads, so the public listener is HTTPS on 443 only. For a throwaway dev stack, `-c insecureDevCidr=<your-ip>/32` serves plain HTTP reachable only from that range (/24 or narrower).

## Options (`-c key=value`)

| Key | Default | |
|---|---|---|
| `certificateArn` | none | ACM certificate for the ingest listener |
| `searchInstanceType`, `searchDataNodes`, `searchVolumeGiB` | `m7g.medium.search`, 2, 20 | `t3.small.search` is fine for dev |
| `dbInstanceClass` | `t4g.micro` | control-plane RDS |
| `consoleUrl` | `https://app.bindle.ai` | OAuth callback and sign-out target |
| `authDomainPrefix` | | Cognito hosted UI prefix, globally unique |
| `consoleCertificateArn` | `certificateArn` | ACM certificate for the console's host (`consoleUrl`). Without any certificate the console is not deployed (Cognito needs https callbacks) |
| `ingestUrl` | `https://ingest.bindle.ai` | shown on the console's Connect page |
| `alertsEmailFrom` | none | sender for email alerts; its domain must be a verified SES identity (not created by the stack) |
| `minTasks`, `maxTasks` | 2, 20 | ingest autoscaling |
| `goproxy` | `https://proxy.golang.org,direct` | where the backend image build downloads Go modules; use `direct` on networks that block proxy.golang.org (or set `BINDLE_GOPROXY`) |

All services run on ARM (Graviton) Fargate tasks; `cdk deploy` builds the images for `linux/arm64`, natively on Apple Silicon and with emulation on x86 machines. Docker must be running. Go dependencies are downloaded during the image build (nothing is vendored).

## After deploy

1. Run the `MigrateCommand` output (an `aws ecs run-task` command): OpenSearch indices, retention and users; control-plane schema and roles.
2. Point `ingest.bindle.ai` at the `IngestEndpoint` output, and the console's host (`consoleUrl`) at `ConsoleEndpoint`.
3. Open the console URL signed out and check you're sent to the Cognito sign-in page before sharing it.
