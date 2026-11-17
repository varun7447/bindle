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
  dbInstanceClass: app.node.tryGetContext('dbInstanceClass'),
  dbMultiAz: app.node.tryGetContext('dbMultiAz') === 'true',
});

cdk.Tags.of(app).add('project', 'bindle');
