import * as path from 'path';
import * as cdk from 'aws-cdk-lib';
import { Construct } from 'constructs';
import * as acm from 'aws-cdk-lib/aws-certificatemanager';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as ecs from 'aws-cdk-lib/aws-ecs';
import * as ecsPatterns from 'aws-cdk-lib/aws-ecs-patterns';
import * as elbv2 from 'aws-cdk-lib/aws-elasticloadbalancingv2';
import * as iam from 'aws-cdk-lib/aws-iam';
import * as logs from 'aws-cdk-lib/aws-logs';
import * as cognito from 'aws-cdk-lib/aws-cognito';
import * as kinesis from 'aws-cdk-lib/aws-kinesis';
import * as opensearch from 'aws-cdk-lib/aws-opensearchservice';
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
  /** OpenSearch data node type. Default m7g.medium.search; t3.small.search for dev. */
  readonly searchInstanceType?: string;
  /** Data nodes, spread over two Availability Zones. Default 2. */
  readonly searchDataNodes?: number;
  /** EBS gp3 volume per data node, GiB. Default 20. */
  readonly searchVolumeGiB?: number;
  /** Control-plane RDS instance class. Default t4g.micro. */
  readonly dbInstanceClass?: string;
  /** Public URL of the console (OAuth callback and sign-out target). */
  readonly consoleUrl?: string;
  /** Cognito hosted UI domain prefix; must be globally unique. */
  readonly authDomainPrefix?: string;
}

/**
 * Bindle data plane, v0:
 *   ALB (HTTPS :443 only) -> Fargate "bindle-ingest" (OTLP/HTTP :4318) -> OpenSearch Service
 *
 * The OpenSearch domain sits in isolated subnets (no internet route), is
 * HTTPS-only with fine-grained access control, and accepts connections
 * only from the ingest service and the setup task. Index setup and the
 * least-privilege users are created by the bindle-migrate task (see outputs).
 *
 * Control plane: RDS PostgreSQL (tenants, users, ingest API keys) in the
 * same isolated subnets, TLS required, reachable only from ingest and the
 * setup task. Console sign-in: an Amazon Cognito user pool with the hosted
 * UI. The API and console services are not deployed by this stack yet.
 *
 * Large prompts/outputs (over 32 KiB) are stored in an S3 bucket (SSE,
 * public access blocked, TLS only, 30-day expiry); ingest may only put.
 * Pipeline: ingest puts each request on a Kinesis stream; bindle-processor
 * (a private Fargate service) consumes it into OpenSearch.
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
        { name: 'data', subnetType: ec2.SubnetType.PRIVATE_ISOLATED },
      ],
    });

    // ---- OpenSearch ----------------------------------------------------------
    // FGAC passwords must include upper, lower, digit and special characters.
    const strong = (username: string) => ({
      secretStringTemplate: JSON.stringify({ username }),
      generateStringKey: 'password',
      passwordLength: 40,
      requireEachIncludedType: true,
      excludeCharacters: `"'\\/@:\` `,
    });
    const searchAdmin = new secretsmanager.Secret(this, 'SearchAdmin', {
      secretName: 'bindle/search-admin',
      description: 'OpenSearch FGAC master user; used only by the setup task',
      generateSecretString: strong('bindle_admin'),
    });
    const searchWriter = new secretsmanager.Secret(this, 'SearchWriter', {
      secretName: 'bindle/search-writer', generateSecretString: strong('bindle_writer'),
    });
    const searchReader = new secretsmanager.Secret(this, 'SearchReader', {
      secretName: 'bindle/search-reader', generateSecretString: strong('bindle_reader'),
    });
    const searchSg = new ec2.SecurityGroup(this, 'SearchSg', { vpc, description: 'Bindle OpenSearch', allowAllOutbound: false });
    const search = new opensearch.Domain(this, 'Search', {
      version: opensearch.EngineVersion.OPENSEARCH_3_7,
      vpc,
      vpcSubnets: [{ subnetType: ec2.SubnetType.PRIVATE_ISOLATED }],
      securityGroups: [searchSg],
      capacity: {
        dataNodes: props.searchDataNodes ?? 2,
        dataNodeInstanceType: props.searchInstanceType ?? 'm7g.medium.search',
        multiAzWithStandbyEnabled: false,
      },
      zoneAwareness: { enabled: true, availabilityZoneCount: 2 },
      ebs: { volumeSize: props.searchVolumeGiB ?? 20, volumeType: ec2.EbsDeviceVolumeType.GP3 },
      encryptionAtRest: { enabled: true },
      nodeToNodeEncryption: true,
      enforceHttps: true,
      tlsSecurityPolicy: opensearch.TLSSecurityPolicy.TLS_1_2_PFS,
      fineGrainedAccessControl: {
        masterUserName: 'bindle_admin',
        masterUserPassword: searchAdmin.secretValueFromJson('password'),
      },
      logging: { appLogEnabled: true },
      removalPolicy: cdk.RemovalPolicy.RETAIN,
    });
    // Reachability is limited by the VPC and security group; who may do
    // what is decided by fine-grained access control (users and roles
    // created by bindle-migrate), which is why the domain policy admits
    // any principal that can reach the endpoint.
    search.addAccessPolicies(new iam.PolicyStatement({
      actions: ['es:ESHttp*'],
      principals: [new iam.AnyPrincipal()],
      resources: [`${search.domainArn}/*`],
    }));
    const searchUrl = `https://${search.domainEndpoint}`;

    // ---- control plane (PostgreSQL) -------------------------------------------
    const dbSg = new ec2.SecurityGroup(this, 'DbSg', { vpc, description: 'Bindle control plane PostgreSQL', allowAllOutbound: false });
    const pgEngine = rds.DatabaseInstanceEngine.postgres({ version: rds.PostgresEngineVersion.VER_17 });
    const db = new rds.DatabaseInstance(this, 'ControlDb', {
      engine: pgEngine,
      instanceType: new ec2.InstanceType(props.dbInstanceClass ?? 't4g.micro'),
      multiAz: false,
      vpc,
      vpcSubnets: { subnetType: ec2.SubnetType.PRIVATE_ISOLATED },
      securityGroups: [dbSg],
      databaseName: 'bindle',
      credentials: rds.Credentials.fromGeneratedSecret('bindle_owner', { secretName: 'bindle/control-db-owner' }),
      parameterGroup: new rds.ParameterGroup(this, 'ControlDbParams', {
        engine: pgEngine,
        parameters: { 'rds.force_ssl': '1', timezone: 'UTC' },
      }),
      allocatedStorage: 20,
      maxAllocatedStorage: 100,
      storageType: rds.StorageType.GP3,
      storageEncrypted: true,
      backupRetention: cdk.Duration.days(7),
      deletionProtection: true,
      removalPolicy: cdk.RemovalPolicy.SNAPSHOT,
      publiclyAccessible: false,
      autoMinorVersionUpgrade: true,
      cloudwatchLogsExports: ['postgresql'],
      cloudwatchLogsRetention: logs.RetentionDays.ONE_MONTH,
    });
    const rolePassword = () => ({ excludePunctuation: true, passwordLength: 40 });
    const dbIngest = new secretsmanager.Secret(this, 'ControlDbIngest', { secretName: 'bindle/control-db-ingest', generateSecretString: rolePassword() });
    const dbProcessor = new secretsmanager.Secret(this, 'ControlDbProcessor', { secretName: 'bindle/control-db-processor', generateSecretString: rolePassword() });
    const dbApi = new secretsmanager.Secret(this, 'ControlDbApi', { secretName: 'bindle/control-db-api', generateSecretString: rolePassword() });
    // libpq settings; TLS verified against the RDS CA bundle in the image.
    const dbEnv = {
      PGHOST: db.dbInstanceEndpointAddress,
      PGPORT: db.dbInstanceEndpointPort,
      PGDATABASE: 'bindle',
      PGSSLMODE: 'verify-full',
      PGSSLROOTCERT: '/etc/bindle/rds-ca.pem',
      PGCONNECT_TIMEOUT: '10',
    };

    // ---- console sign-in (Cognito) --------------------------------------------
    const consoleUrl = (props.consoleUrl ?? 'https://app.bindle.ai').replace(/\/+$/, '');
    const userPool = new cognito.UserPool(this, 'Users', {
      userPoolName: 'bindle-users',
      selfSignUpEnabled: true,
      signInAliases: { email: true },
      autoVerify: { email: true },
      standardAttributes: { email: { required: true, mutable: true } },
      passwordPolicy: { minLength: 12, requireLowercase: true, requireUppercase: true, requireDigits: true, requireSymbols: false },
      mfa: cognito.Mfa.OPTIONAL,
      mfaSecondFactor: { otp: true, sms: false },
      accountRecovery: cognito.AccountRecovery.EMAIL_ONLY,
      featurePlan: cognito.FeaturePlan.ESSENTIALS,
      deletionProtection: true,
      removalPolicy: cdk.RemovalPolicy.RETAIN,
    });
    const authDomain = userPool.addDomain('HostedUi', {
      cognitoDomain: { domainPrefix: props.authDomainPrefix ?? 'bindle' },
    });
    const consoleClient = userPool.addClient('Console', {
      userPoolClientName: 'bindle-console',
      generateSecret: true,
      authFlows: {}, // no direct password APIs: sign-in only through the hosted UI
      oAuth: {
        flows: { authorizationCodeGrant: true },
        scopes: [cognito.OAuthScope.OPENID, cognito.OAuthScope.EMAIL],
        callbackUrls: [`${consoleUrl}/auth/callback`],
        logoutUrls: [`${consoleUrl}/`],
      },
      supportedIdentityProviders: [cognito.UserPoolClientIdentityProvider.COGNITO],
      preventUserExistenceErrors: true,
      accessTokenValidity: cdk.Duration.hours(1),
      idTokenValidity: cdk.Duration.hours(1),
      refreshTokenValidity: cdk.Duration.days(30),
      enableTokenRevocation: true,
    });

    const image = ecs.ContainerImage.fromAsset(path.join(__dirname, '../../../services'));

    // Ingest -> processor. On-demand capacity: no shards to size up front.
    const steps = new kinesis.Stream(this, 'Steps', {
      streamMode: kinesis.StreamMode.ON_DEMAND,
      encryption: kinesis.StreamEncryption.MANAGED,
      retentionPeriod: cdk.Duration.hours(48),
    });

    const payloads = new s3.Bucket(this, 'Payloads', {
      encryption: s3.BucketEncryption.S3_MANAGED,
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      enforceSSL: true,
      lifecycleRules: [
        { expiration: cdk.Duration.days(30) },
        // Batches spooled for the processor are only needed until processed.
        { prefix: 'spool/', expiration: cdk.Duration.days(2) },
      ],
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
          PGUSER: 'bindle_ingest',
          // Ingest puts each request on the stream; bindle-processor writes OpenSearch.
          BINDLE_SINK: 'stream',
          BINDLE_STREAM_NAME: steps.streamName,
          BINDLE_PRICING_FILE: '/etc/bindle/pricing.json',
          BINDLE_RATE_LIMIT_SPANS_PER_SEC: '2000',
          // Batches over a Kinesis record's size are spooled here (spool/...).
          BINDLE_PAYLOAD_STORE: `s3://${payloads.bucketName}`,
        },
        secrets: {
          PGPASSWORD: ecs.Secret.fromSecretsManager(dbIngest),
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

    steps.grant(ingest.taskDefinition.taskRole, 'kinesis:PutRecord');
    ingest.taskDefinition.taskRole.addToPrincipalPolicy(new iam.PolicyStatement({
      actions: ['s3:PutObject'],
      resources: [payloads.arnForObjects('spool/*')],
    }));
    db.connections.allowDefaultPortFrom(ingest.service, 'bindle-ingest');

    // One-off setup task: run it after each deploy (command in the
    // MigrateCommand output). Same image, different entrypoint.
    const migrateTask = new ecs.FargateTaskDefinition(this, 'MigrateTask', { cpu: 256, memoryLimitMiB: 512 });
    migrateTask.addContainer('migrate', {
      image,
      entryPoint: ['/bindle-migrate'],
      environment: { BINDLE_SEARCH_URL: searchUrl, BINDLE_SEARCH_REPLICAS: '1', ...dbEnv },
      secrets: {
        PGUSER: ecs.Secret.fromSecretsManager(db.secret!, 'username'),
        PGPASSWORD: ecs.Secret.fromSecretsManager(db.secret!, 'password'),
        BINDLE_CONTROL_API_PASSWORD: ecs.Secret.fromSecretsManager(dbApi),
        BINDLE_CONTROL_INGEST_PASSWORD: ecs.Secret.fromSecretsManager(dbIngest),
        BINDLE_CONTROL_PROCESSOR_PASSWORD: ecs.Secret.fromSecretsManager(dbProcessor),
        BINDLE_SEARCH_ADMIN_USER: ecs.Secret.fromSecretsManager(searchAdmin, 'username'),
        BINDLE_SEARCH_ADMIN_PASSWORD: ecs.Secret.fromSecretsManager(searchAdmin, 'password'),
        BINDLE_SEARCH_WRITER_PASSWORD: ecs.Secret.fromSecretsManager(searchWriter, 'password'),
        BINDLE_SEARCH_READER_PASSWORD: ecs.Secret.fromSecretsManager(searchReader, 'password'),
      },
      logging: ecs.LogDrivers.awsLogs({ streamPrefix: 'migrate', logRetention: logs.RetentionDays.ONE_MONTH }),
    });
    const migrateSg = new ec2.SecurityGroup(this, 'MigrateSg', { vpc, description: 'bindle-migrate task' });
    searchSg.addIngressRule(ec2.Peer.securityGroupId(migrateSg.securityGroupId), ec2.Port.tcp(443), 'bindle-migrate');
    db.connections.allowDefaultPortFrom(migrateSg, 'bindle-migrate');
    const appSubnets = vpc.selectSubnets({ subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS }).subnetIds;

    new cdk.CfnOutput(this, 'IngestEndpoint', {
      value: `${certificate ? 'https' : 'http'}://${ingest.loadBalancer.loadBalancerDnsName}`,
      description: 'Point a CNAME for ingest.bindle.ai here',
    });
    // ---- processor: stream -> OpenSearch ---------------------------------------
    const processorTask = new ecs.FargateTaskDefinition(this, 'ProcessorTask', { cpu: 512, memoryLimitMiB: 1024 });
    processorTask.addContainer('processor', {
      image,
      entryPoint: ['/bindle-processor'],
      environment: {
        ...dbEnv,
        PGUSER: 'bindle_processor',
        BINDLE_STREAM_NAME: steps.streamName,
        BINDLE_SEARCH_URL: searchUrl,
        BINDLE_SEARCH_USER: 'bindle_writer',
        // Payloads are stored under payloads/... and spooled batches read from spool/...
        BINDLE_PAYLOAD_STORE: `s3://${payloads.bucketName}`,
      },
      secrets: {
        BINDLE_SEARCH_PASSWORD: ecs.Secret.fromSecretsManager(searchWriter, 'password'),
        PGPASSWORD: ecs.Secret.fromSecretsManager(dbProcessor),
      },
      logging: ecs.LogDrivers.awsLogs({ streamPrefix: 'processor', logRetention: logs.RetentionDays.ONE_MONTH }),
    });
    const processor = new ecs.FargateService(this, 'Processor', {
      cluster,
      taskDefinition: processorTask,
      desiredCount: 1,
      minHealthyPercent: 100,
      maxHealthyPercent: 200,
      vpcSubnets: { subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS },
      circuitBreaker: { rollback: true },
    });
    processor.autoScaleTaskCount({ minCapacity: 1, maxCapacity: 4 }).scaleOnCpuUtilization('Cpu', { targetUtilizationPercent: 60 });
    steps.grantRead(processorTask.taskRole);
    processorTask.taskRole.addToPrincipalPolicy(new iam.PolicyStatement({
      actions: ['s3:GetObject'], resources: [payloads.arnForObjects('spool/*')],
    }));
    processorTask.taskRole.addToPrincipalPolicy(new iam.PolicyStatement({
      actions: ['s3:PutObject'], resources: [payloads.arnForObjects('tenants/*')],
    }));
    searchSg.addIngressRule(ec2.Peer.securityGroupId(processor.connections.securityGroups[0].securityGroupId), ec2.Port.tcp(443), 'bindle-processor');
    db.connections.allowDefaultPortFrom(processor, 'bindle-processor');

    new cdk.CfnOutput(this, 'PayloadBucket', { value: payloads.bucketName });
    new cdk.CfnOutput(this, 'StepsStream', { value: steps.streamName });
    new cdk.CfnOutput(this, 'SearchEndpoint', { value: searchUrl });
    new cdk.CfnOutput(this, 'ControlDbEndpoint', { value: db.dbInstanceEndpointAddress });
    new cdk.CfnOutput(this, 'CognitoIssuer', {
      description: 'BINDLE_COGNITO_ISSUER for bindle-api',
      value: `https://cognito-idp.${this.region}.amazonaws.com/${userPool.userPoolId}`,
    });
    new cdk.CfnOutput(this, 'CognitoDomain', {
      description: 'BINDLE_COGNITO_DOMAIN for the console',
      value: authDomain.baseUrl(),
    });
    new cdk.CfnOutput(this, 'ConsoleClientId', {
      description: 'BINDLE_COGNITO_CLIENT_ID for the console and bindle-api (secret: aws cognito-idp describe-user-pool-client)',
      value: consoleClient.userPoolClientId,
    });
    new cdk.CfnOutput(this, 'MigrateCommand', {
      description: 'Run after each deploy: OpenSearch indices, retention policy and users; control-plane schema and roles',
      value: cdk.Fn.join('', [
        'aws ecs run-task --cluster ', cluster.clusterName,
        ' --launch-type FARGATE --task-definition ', migrateTask.taskDefinitionArn,
        ' --network-configuration "awsvpcConfiguration={subnets=[', cdk.Fn.join(',', appSubnets),
        '],securityGroups=[', migrateSg.securityGroupId, '],assignPublicIp=DISABLED}"',
      ]),
    });
  }
}
