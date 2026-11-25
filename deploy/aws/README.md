# AWS deployment (CDK)

The stack: VPC, Fargate ingest behind an HTTPS-only ALB, the console behind its own HTTPS ALB (Cognito sign-in), a private API reachable only from the console (`api.bindle.internal`), Kinesis stream, Fargate processor and alerts services, OpenSearch Service domain, RDS PostgreSQL (control plane), Cognito user pool, S3 payload bucket, Secrets Manager secrets, a KMS key for secrets stored in the database, and a one-off setup task. **These resources cost money once deployed.**

```bash
cd deploy/aws && npm install
npx cdk synth -c insecureDevCidr=203.0.113.0/24     # check only (or `make synth` from the repo root)
npx cdk deploy -c certificateArn=arn:aws:acm:... -c consoleUrl=https://bindle.example.com -c ingestUrl=https://ingest.example.com
```

Synth fails without a certificate: ingest carries API keys and agent payloads, so the public listener is HTTPS on 443 only. For a throwaway dev stack, `-c insecureDevCidr=<your-ip>/32` serves plain HTTP reachable only from that range (/24 or narrower).

## Options (`-c key=value`)

| Key | Default | |
|---|---|---|
| `certificateArn` | none | ACM certificate for the ingest listener |
| `searchInstanceType`, `searchDataNodes`, `searchVolumeGiB` | `m7g.medium.search`, 2, 20 | `t3.small.search` is fine for dev |
| `dbInstanceClass` | `t4g.micro` | control-plane RDS |
| `consoleUrl` | none; required when the console is deployed | OAuth callback and sign-out target, on a host you control |
| `authDomainPrefix` | | Cognito hosted UI prefix, globally unique |
| `consoleCertificateArn` | `certificateArn` | ACM certificate for the console's host (`consoleUrl`). Without any certificate the console is not deployed (Cognito needs https callbacks) |
| `ingestUrl` | none (the Connect page shows a placeholder) | your public ingest URL, shown on the console's Connect page |
| `alertsEmailFrom` | none | sender for email alerts; its domain must be a verified SES identity (not created by the stack) |
| `minTasks`, `maxTasks` | 2, 20 | ingest autoscaling |
| `allowedCidrs` | none (open to the internet) | comma-separated IPv4 ranges (/8 to /32) that may reach the ingest and console load balancers, e.g. your VPN egress |
| `selfSignUp` | `true` | `false`: only users an admin creates (`aws cognito-idp admin-create-user`) can sign in |
| `goproxy` | `https://proxy.golang.org,direct` | where the backend image build downloads Go modules; use `direct` on networks that block proxy.golang.org (or set `BINDLE_GOPROXY`) |

**Secrets in the database** (alert webhook and Slack URLs, signing secrets) are encrypted with a customer-managed KMS key (`alias/bindle-secrets`, yearly rotation, kept if the stack is deleted). Each secret is bound to its workspace and row through the KMS encryption context, and each service may only encrypt or decrypt what it needs. The key costs $1 a month, rising to $3 after its first two yearly rotations, plus KMS request charges (see the KMS pricing page); decryptions are cached for 5 minutes, so requests stay low.

All services run on ARM (Graviton) Fargate tasks; `cdk deploy` builds the images for `linux/arm64`, natively on Apple Silicon and with emulation on x86 machines. Docker must be running. Go dependencies are downloaded during the image build (nothing is vendored).

## After deploy

1. Run the `MigrateCommand` output (an `aws ecs run-task` command): OpenSearch indices, retention and users; control-plane schema and roles.
2. Point your ingest host (`ingestUrl`) at the `IngestEndpoint` output, and the console's host (`consoleUrl`) at `ConsoleEndpoint`.
3. Open the console URL signed out and check you're sent to the Cognito sign-in page before sharing it.
4. Upgrading a stack created before secrets were encrypted: run the `MigrateCommand` once more with the entrypoint `/bindle-admin` and the command `secrets encrypt` (add `--overrides '{"containerOverrides":[{"name":"migrate","entryPoint":["/bindle-admin"],"command":["secrets","encrypt"]}]}'`). It encrypts the old rows and clears their plaintext; running it again changes nothing.
