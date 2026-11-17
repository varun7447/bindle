#!/usr/bin/env node
import * as cdk from 'aws-cdk-lib';
import { BindleStack } from '../lib/bindle-stack';

const app = new cdk.App();

new BindleStack(app, 'BindleDataPlane', {
  env: {
    account: process.env.CDK_DEFAULT_ACCOUNT,
    region: process.env.CDK_DEFAULT_REGION ?? 'us-west-2',
  },
  certificateArn: app.node.tryGetContext('certificateArn'),
  insecureDevCidr: app.node.tryGetContext('insecureDevCidr'),
  minTasks: Number(app.node.tryGetContext('minTasks') ?? 2),
  maxTasks: Number(app.node.tryGetContext('maxTasks') ?? 20),
  searchInstanceType: app.node.tryGetContext('searchInstanceType'),
  searchDataNodes: app.node.tryGetContext('searchDataNodes') ? Number(app.node.tryGetContext('searchDataNodes')) : undefined,
  searchVolumeGiB: app.node.tryGetContext('searchVolumeGiB') ? Number(app.node.tryGetContext('searchVolumeGiB')) : undefined,
});

cdk.Tags.of(app).add('project', 'bindle');
