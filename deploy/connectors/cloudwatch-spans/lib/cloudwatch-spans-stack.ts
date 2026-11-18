import * as path from 'path';
import * as cdk from 'aws-cdk-lib';
import * as lambda from 'aws-cdk-lib/aws-lambda';
import * as logs from 'aws-cdk-lib/aws-logs';
import * as destinations from 'aws-cdk-lib/aws-logs-destinations';
import * as secretsmanager from 'aws-cdk-lib/aws-secretsmanager';
import { Construct } from 'constructs';

export interface CloudWatchSpansProps extends cdk.StackProps {
  /** Bindle ingest base URL, https only, for example https://ingest.example.com */
  bindleEndpoint: string;
  /** Complete ARN of a Secrets Manager secret holding the Bindle ingest key. */
  apiKeySecretArn: string;
  /** Log group with the spans. Default: aws/spans (CloudWatch Transaction Search). */
  logGroupName?: string;
  /** Comma-separated span name prefixes to forward. Default: AgentCore. */
  spanNamePrefixes?: string;
}

/**
 * Deployed in the customer's AWS account: forwards AWS service spans
 * (AgentCore Gateway, Policy, Identity, built-in tools) from CloudWatch Logs
 * to Bindle. Nothing here listens on the network; the function only makes
 * outbound HTTPS calls to the Bindle endpoint.
 */
export class CloudWatchSpansStack extends cdk.Stack {
  constructor(scope: Construct, id: string, props: CloudWatchSpansProps) {
    super(scope, id, props);

    if (!/^https:\/\/[^/\s]+/.test(props.bindleEndpoint)) {
      throw new Error('Set -c bindleEndpoint=https://<your Bindle ingest host>');
    }
    if (!props.apiKeySecretArn.startsWith('arn:')) {
      throw new Error('Set -c apiKeySecretArn=<complete ARN of the secret holding the ingest key>');
    }
    const prefixes = props.spanNamePrefixes ?? 'AgentCore.';
    const logGroupName = props.logGroupName ?? 'aws/spans';

    const secret = secretsmanager.Secret.fromSecretCompleteArn(this, 'IngestKey', props.apiKeySecretArn);

    const fnLogs = new logs.LogGroup(this, 'ForwarderLogs', {
      retention: logs.RetentionDays.ONE_MONTH,
      removalPolicy: cdk.RemovalPolicy.DESTROY,
    });

    const fn = new lambda.Function(this, 'Forwarder', {
      description: 'Forwards AWS service spans from CloudWatch Logs to Bindle',
      runtime: lambda.Runtime.PYTHON_3_12,
      architecture: lambda.Architecture.ARM_64,
      handler: 'handler.handler',
      code: lambda.Code.fromAsset(path.join(__dirname, '..', 'forwarder'), { exclude: ['test_*.py', '__pycache__'] }),
      memorySize: 256,
      timeout: cdk.Duration.seconds(60),
      logGroup: fnLogs,
      environment: {
        BINDLE_ENDPOINT: props.bindleEndpoint,
        BINDLE_API_KEY_SECRET_ARN: props.apiKeySecretArn,
        SPAN_NAME_PREFIXES: prefixes,
      },
    });
    // Least privilege: read this one secret; the basic execution role covers its own logs.
    secret.grantRead(fn);

    // Pre-filter in CloudWatch so only matching spans invoke the function
    // (and incur cost). The function filters again, so this is only an
    // optimization. JSON filter patterns allow a trailing * wildcard.
    const names = prefixes.split(',').map((p) => p.trim()).filter(Boolean);
    const pattern = names.map((p) => `$.name = "${p.replace(/"/g, '')}*"`).join(' || ');
    new logs.SubscriptionFilter(this, 'Spans', {
      logGroup: logs.LogGroup.fromLogGroupName(this, 'SpansLogGroup', logGroupName),
      destination: new destinations.LambdaDestination(fn),
      filterPattern: logs.FilterPattern.literal(`{ ${pattern} }`),
    });

    new cdk.CfnOutput(this, 'ForwarderFunction', { value: fn.functionName });
  }
}
