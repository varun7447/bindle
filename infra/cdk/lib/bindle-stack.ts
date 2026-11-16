import * as path from 'path';
import * as cdk from 'aws-cdk-lib';
import { Construct } from 'constructs';
import * as acm from 'aws-cdk-lib/aws-certificatemanager';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as ecs from 'aws-cdk-lib/aws-ecs';
import * as ecsPatterns from 'aws-cdk-lib/aws-ecs-patterns';
import * as elbv2 from 'aws-cdk-lib/aws-elasticloadbalancingv2';
import * as logs from 'aws-cdk-lib/aws-logs';
import * as s3 from 'aws-cdk-lib/aws-s3';
import * as secretsmanager from 'aws-cdk-lib/aws-secretsmanager';

export interface BindleStackProps extends cdk.StackProps {
  /** ACM certificate ARN for ingest.bindle.ai. Required unless insecureDevCidr is set. */
  readonly certificateArn?: string;
  /**
   * Dev only: serve plain HTTP (no certificate) and allow it only from this
   * CIDR (e.g. "203.0.113.7/32"). API keys and payloads travel unencrypted,
   * so this must never be a wide range.
   */
  readonly insecureDevCidr?: string;
  readonly minTasks?: number;
  readonly maxTasks?: number;
}

/**
 * Bindle data plane, v0:
 *   ALB (HTTPS :443 only) -> Fargate "bindle-ingest" (OTLP/HTTP :4318) -> ClickHouse Cloud
 *
 * Also provisioned for payload offload (step 5, not yet used by the service):
 *   - S3 bucket for large prompt/output payloads
 * The Kinesis stream for the ingest -> processor split is added in step 6,
 * together with the processor, so it does not cost money while unused.
 */
export class BindleStack extends cdk.Stack {
  constructor(scope: Construct, id: string, props: BindleStackProps = {}) {
    super(scope, id, props);

    if (!props.certificateArn && !props.insecureDevCidr) {
      throw new Error(
        'BindleStack: pass -c certificateArn=arn:aws:acm:... (HTTPS). For a dev-only plain-HTTP ' +
          'stack reachable from one address, pass -c insecureDevCidr=<your-ip>/32 instead.',
      );
    }
    if (props.certificateArn && props.insecureDevCidr) {
      throw new Error('BindleStack: certificateArn and insecureDevCidr are mutually exclusive');
    }
    if (props.insecureDevCidr) {
      const [, bits] = props.insecureDevCidr.split('/');
      if (!bits || Number(bits) < 24) {
        throw new Error(`BindleStack: insecureDevCidr must be /24 or narrower, got ${props.insecureDevCidr}`);
      }
    }

    const vpc = new ec2.Vpc(this, 'Vpc', { maxAzs: 2, natGateways: 1 });

    // ClickHouse Cloud connection. After deploy, set url/user in this secret;
    // the password is generated here and must be set on the ClickHouse user.
    const clickhouse = new secretsmanager.Secret(this, 'ClickHouse', {
      secretName: 'bindle/clickhouse',
      generateSecretString: {
        secretStringTemplate: JSON.stringify({ url: 'https://CHANGE-ME.clickhouse.cloud:8443', user: 'bindle_ingest' }),
        generateStringKey: 'password',
        excludePunctuation: true,
      },
    });

    // Dev bootstrap for ingest keys ("key=tenant,key2=tenant2"). Replaced by
    // the control-plane key service once self-serve sign-up exists.
    const ingestKeys = new secretsmanager.Secret(this, 'IngestKeys', {
      secretName: 'bindle/ingest-api-keys',
      description: 'Comma-separated key=tenant pairs for bindle-ingest (v0 bootstrap)',
    });

    const payloads = new s3.Bucket(this, 'Payloads', {
      encryption: s3.BucketEncryption.S3_MANAGED,
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      enforceSSL: true,
      lifecycleRules: [{ expiration: cdk.Duration.days(30) }],
      removalPolicy: cdk.RemovalPolicy.RETAIN,
    });

    const cluster = new ecs.Cluster(this, 'Cluster', { vpc, containerInsightsV2: ecs.ContainerInsights.ENABLED });

    const certificate = props.certificateArn
      ? acm.Certificate.fromCertificateArn(this, 'Cert', props.certificateArn)
      : undefined;

    const ingest = new ecsPatterns.ApplicationLoadBalancedFargateService(this, 'Ingest', {
      cluster,
      cpu: 512,
      memoryLimitMiB: 1024,
      desiredCount: props.minTasks ?? 2,
      // Keep full capacity during deploys: start new tasks before stopping old ones.
      minHealthyPercent: 100,
      maxHealthyPercent: 200,
      // Public ingest: customers' agents send from anywhere and authenticate
      // with API keys. HTTPS only (443); no port-80 redirect listener, since
      // OTLP exporters should be configured with https:// directly.
      publicLoadBalancer: true,
      certificate,
      protocol: certificate ? elbv2.ApplicationProtocol.HTTPS : elbv2.ApplicationProtocol.HTTP,
      redirectHTTP: false,
      // In insecure dev mode the listener is opened to one CIDR below, not 0.0.0.0/0.
      openListener: !!certificate,
      circuitBreaker: { rollback: true },
      taskImageOptions: {
        image: ecs.ContainerImage.fromAsset(path.join(__dirname, '../../../services')),
        containerPort: 4318,
        environment: {
          BINDLE_SINK: 'clickhouse',
          BINDLE_PRICING_FILE: '/etc/bindle/pricing.json',
          BINDLE_RATE_LIMIT_SPANS_PER_SEC: '2000',
          BINDLE_PAYLOAD_BUCKET: payloads.bucketName,
        },
        secrets: {
          CLICKHOUSE_URL: ecs.Secret.fromSecretsManager(clickhouse, 'url'),
          CLICKHOUSE_USER: ecs.Secret.fromSecretsManager(clickhouse, 'user'),
          CLICKHOUSE_PASSWORD: ecs.Secret.fromSecretsManager(clickhouse, 'password'),
          BINDLE_API_KEYS: ecs.Secret.fromSecretsManager(ingestKeys),
        },
        logDriver: ecs.LogDrivers.awsLogs({ streamPrefix: 'ingest', logRetention: logs.RetentionDays.ONE_MONTH }),
      },
    });

    if (props.insecureDevCidr) {
      ingest.loadBalancer.connections.allowFrom(
        ec2.Peer.ipv4(props.insecureDevCidr),
        ec2.Port.tcp(80),
        'Dev-only plain HTTP ingest from one address',
      );
    }

    ingest.targetGroup.configureHealthCheck({ path: '/healthz', healthyHttpCodes: '200' });
    ingest.targetGroup.setAttribute('deregistration_delay.timeout_seconds', '30');

    const scaling = ingest.service.autoScaleTaskCount({
      minCapacity: props.minTasks ?? 2,
      maxCapacity: props.maxTasks ?? 20,
    });
    scaling.scaleOnCpuUtilization('Cpu', { targetUtilizationPercent: 60 });
    scaling.scaleOnRequestCount('Requests', {
      requestsPerTarget: 2000,
      targetGroup: ingest.targetGroup,
    });

    payloads.grantWrite(ingest.taskDefinition.taskRole);

    new cdk.CfnOutput(this, 'IngestEndpoint', {
      value: `${certificate ? 'https' : 'http'}://${ingest.loadBalancer.loadBalancerDnsName}`,
      description: 'Point a CNAME for ingest.bindle.ai here',
    });
    new cdk.CfnOutput(this, 'PayloadBucket', { value: payloads.bucketName });
  }
}
