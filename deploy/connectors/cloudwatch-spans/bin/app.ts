#!/usr/bin/env node
import * as cdk from 'aws-cdk-lib';
import { CloudWatchSpansStack } from '../lib/cloudwatch-spans-stack';

const app = new cdk.App();
const ctx = (k: string) => app.node.tryGetContext(k) as string | undefined;

new CloudWatchSpansStack(app, 'BindleCloudWatchSpans', {
  env: { account: process.env.CDK_DEFAULT_ACCOUNT, region: process.env.CDK_DEFAULT_REGION },
  bindleEndpoint: ctx('bindleEndpoint') ?? '',
  apiKeySecretArn: ctx('apiKeySecretArn') ?? '',
  logGroupName: ctx('logGroupName'),
  spanNamePrefixes: ctx('spanNamePrefixes'),
});

cdk.Tags.of(app).add('project', 'bindle');
