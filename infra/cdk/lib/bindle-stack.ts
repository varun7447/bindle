import * as path from 'path';
import * as cdk from 'aws-cdk-lib';
import { Construct } from 'constructs';
import * as acm from 'aws-cdk-lib/aws-certificatemanager';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as ecs from 'aws-cdk-lib/aws-ecs';
import * as ecsPatterns from 'aws-cdk-lib/aws-ecs-patterns';
import * as elbv2 from 'aws-cdk-lib/aws-elasticloadbalancingv2';
import * as logs from 'aws-cdk-lib/aws-logs';
import * as rds from 'aws-cdk-lib/aws-rds';
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
  /** RDS instance class, e.g. "t4g.micro" (default) or "m7g.large". */
  readonly dbInstanceClass?: string;
  /** Run the database in two Availability Zones (standby). Default false. */
  readonly dbMultiAz?: boolean;
}

/**
 * Bindle data plane, v0:
 *   ALB (HTTPS :443 only) -> Fargate "bindle-ingest" (OTLP/HTTP :4318) -> RDS PostgreSQL
 *
 * The database sits in isolated subnets (no internet route) and accepts
 * connections only from the ingest service and the migrate task, over TLS.
 * Schema and roles are created by the bindle-migrate task (see outputs).
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

    const vpc = new ec2.Vpc(this, 'Vpc', {
      maxAzs: 2,
      natGateways: 1,
      subnetConfiguration: [
        { name: 'public', subnetType: ec2.SubnetType.PUBLIC },
        { name: 'app', subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS },
        { name: 'db', subnetType: ec2.SubnetType.PRIVATE_ISOLATED },
      ],
    });

    // ---- database ----------------------------------------------------------
    const dbSg = new ec2.SecurityGroup(this, 'DbSg', { vpc, description: 'Bindle PostgreSQL', allowAllOutbound: false });
    const dbParams = new rds.ParameterGroup(this, 'DbParams', {
      engine: rds.DatabaseInstanceEngine.postgres({ version: rds.PostgresEngineVersion.VER_17 }),
      parameters: { 'rds.force_ssl': '1', timezone: 'UTC' },
    });
    const db = new rds.DatabaseInstance(this, 'Db', {
      engine: rds.DatabaseInstanceEngine.postgres({ version: rds.PostgresEngineVersion.VER_17 }),
      instanceType: new ec2.InstanceType(props.dbInstanceClass ?? 't4g.micro'),
      multiAz: props.dbMultiAz ?? false,
      vpc,
      vpcSubnets: { subnetType: ec2.SubnetType.PRIVATE_ISOLATED },
      securityGroups: [dbSg],
      databaseName: 'bindle',
      // Schema owner; only the migrate task uses it.
      credentials: rds.Credentials.fromGeneratedSecret('bindle_owner', { secretName: 'bindle/db-owner' }),
      parameterGroup: dbParams,
      allocatedStorage: 20,
      maxAllocatedStorage: 200,
      storageType: rds.StorageType.GP3,
      storageEncrypted: true,
      backupRetention: cdk.Duration.days(7),
      deletionProtection: true,
      removalPolicy: cdk.RemovalPolicy.SNAPSHOT,
      publiclyAccessible: false,
      autoMinorVersionUpgrade: true,
      enablePerformanceInsights: false,
      cloudwatchLogsExports: ['postgresql'],
      cloudwatchLogsRetention: logs.RetentionDays.ONE_MONTH,
    });
    // App role passwords. bindle-migrate sets them on the roles.
    const dbWriter = new secretsmanager.Secret(this, 'DbWriter', {
      secretName: 'bindle/db-writer',
      generateSecretString: { excludePunctuation: true, passwordLength: 40 },
    });
    const dbReader = new secretsmanager.Secret(this, 'DbReader', {
      secretName: 'bindle/db-reader',
      generateSecretString: { excludePunctuation: true, passwordLength: 40 },
    });
    // libpq-style connection settings; TLS verified against the RDS CA bundle in the image.
    const dbEnv = {
      PGHOST: db.dbInstanceEndpointAddress,
      PGPORT: db.dbInstanceEndpointPort,
      PGDATABASE: 'bindle',
      PGSSLMODE: 'verify-full',
      PGSSLROOTCERT: '/etc/bindle/rds-ca.pem',
      PGCONNECT_TIMEOUT: '10',
    };
    const image = ecs.ContainerImage.fromAsset(path.join(__dirname, '../../../services'));

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
        image,
        containerPort: 4318,
        environment: {
          ...dbEnv,
          PGUSER: 'bindle_writer',
          BINDLE_SINK: 'postgres',
          BINDLE_PRICING_FILE: '/etc/bindle/pricing.json',
          BINDLE_RATE_LIMIT_SPANS_PER_SEC: '2000',
          BINDLE_PAYLOAD_BUCKET: payloads.bucketName,
        },
        secrets: {
          PGPASSWORD: ecs.Secret.fromSecretsManager(dbWriter),
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
    db.connections.allowDefaultPortFrom(ingest.service, 'bindle-ingest');

    // One-off migration task: run it after each deploy that changes the schema
    // (command in the MigrateCommand output). Same image, different entrypoint.
    const migrateTask = new ecs.FargateTaskDefinition(this, 'MigrateTask', { cpu: 256, memoryLimitMiB: 512 });
    migrateTask.addContainer('migrate', {
      image,
      entryPoint: ['/bindle-migrate'],
      environment: dbEnv,
      secrets: {
        PGUSER: ecs.Secret.fromSecretsManager(db.secret!, 'username'),
        PGPASSWORD: ecs.Secret.fromSecretsManager(db.secret!, 'password'),
        BINDLE_DB_WRITER_PASSWORD: ecs.Secret.fromSecretsManager(dbWriter),
        BINDLE_DB_READER_PASSWORD: ecs.Secret.fromSecretsManager(dbReader),
      },
      logging: ecs.LogDrivers.awsLogs({ streamPrefix: 'migrate', logRetention: logs.RetentionDays.ONE_MONTH }),
    });
    const migrateSg = new ec2.SecurityGroup(this, 'MigrateSg', { vpc, description: 'bindle-migrate task' });
    db.connections.allowDefaultPortFrom(migrateSg, 'bindle-migrate');
    const appSubnets = vpc.selectSubnets({ subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS }).subnetIds;

    new cdk.CfnOutput(this, 'IngestEndpoint', {
      value: `${certificate ? 'https' : 'http'}://${ingest.loadBalancer.loadBalancerDnsName}`,
      description: 'Point a CNAME for ingest.bindle.ai here',
    });
    new cdk.CfnOutput(this, 'PayloadBucket', { value: payloads.bucketName });
    new cdk.CfnOutput(this, 'DbEndpoint', { value: db.dbInstanceEndpointAddress });
    new cdk.CfnOutput(this, 'MigrateCommand', {
      description: 'Run after deploy to create roles and schema',
      value: cdk.Fn.join('', [
        'aws ecs run-task --cluster ', cluster.clusterName,
        ' --launch-type FARGATE --task-definition ', migrateTask.taskDefinitionArn,
        ' --network-configuration "awsvpcConfiguration={subnets=[', cdk.Fn.join(',', appSubnets),
        '],securityGroups=[', migrateSg.securityGroupId, '],assignPublicIp=DISABLED}"',
      ]),
    });
  }
}
