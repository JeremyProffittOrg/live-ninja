"""Deploy the trial recorder from GitHub Actions using the configured OIDC role."""
import io
import json
import os
from pathlib import Path
import zipfile

import boto3
from botocore.exceptions import ClientError


def template():
    ref = lambda name: {"Ref": name}
    sub = lambda value: {"Fn::Sub": value}
    return {
        "AWSTemplateFormatVersion": "2010-09-09",
        "Description": "Live Ninja bounded phone trial recordings and download links",
        "Parameters": {
            "CodeBucket": {"Type": "String"}, "CodeKey": {"Type": "String"},
            "TwilioAccount": {"Type": "String"}, "TwilioKeySid": {"Type": "String"},
            "TwilioAuth": {"Type": "String", "NoEcho": True},
            "TwilioKeySecret": {"Type": "String", "NoEcho": True},
        },
        "Resources": {
            "Table": {"Type": "AWS::DynamoDB::Table", "DeletionPolicy": "Retain", "UpdateReplacePolicy": "Retain", "Properties": {
                "TableName": "live-ninja-phone-trial", "BillingMode": "PAY_PER_REQUEST",
                "AttributeDefinitions": [{"AttributeName": "PK", "AttributeType": "S"}, {"AttributeName": "SK", "AttributeType": "S"}],
                "KeySchema": [{"AttributeName": "PK", "KeyType": "HASH"}, {"AttributeName": "SK", "KeyType": "RANGE"}],
                "TimeToLiveSpecification": {"AttributeName": "ttlAt", "Enabled": True},
                "SSESpecification": {"SSEEnabled": True},
            }},
            "Api": {"Type": "AWS::ApiGatewayV2::Api", "Properties": {"Name": "live-ninja-phone-trial", "ProtocolType": "HTTP"}},
            "LogGroup": {"Type": "AWS::Logs::LogGroup", "Properties": {"LogGroupName": "/aws/lambda/live-ninja-phone-trial-recorder", "RetentionInDays": 7}},
            "Role": {"Type": "AWS::IAM::Role", "Properties": {
                "RoleName": "live-ninja-phone-trial-recorder",
                "AssumeRolePolicyDocument": {"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Principal": {"Service": "lambda.amazonaws.com"}, "Action": "sts:AssumeRole"}]},
                "Policies": [{"PolicyName": "phone-trial", "PolicyDocument": {"Version": "2012-10-17", "Statement": [
                    {"Effect": "Allow", "Action": ["logs:CreateLogStream", "logs:PutLogEvents"], "Resource": sub("arn:${AWS::Partition}:logs:${AWS::Region}:${AWS::AccountId}:log-group:/aws/lambda/live-ninja-phone-trial-recorder:*")},
                    {"Effect": "Allow", "Action": ["dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:Query", "dynamodb:TransactWriteItems", "dynamodb:UpdateItem"], "Resource": {"Fn::GetAtt": ["Table", "Arn"]}},
                    {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject"], "Resource": "arn:aws:s3:::live-ninja-deliverables-759775734231/phone-trial/*"},
                ]}}],
            }},
            "Function": {"Type": "AWS::Lambda::Function", "DependsOn": ["LogGroup"], "Properties": {
                "FunctionName": "live-ninja-phone-trial-recorder", "Runtime": "python3.12", "Architectures": ["arm64"],
                "Handler": "recorder.handler", "MemorySize": 512, "Timeout": 60,
                "Role": {"Fn::GetAtt": ["Role", "Arn"]},
                "Code": {"S3Bucket": ref("CodeBucket"), "S3Key": ref("CodeKey")},
                "Environment": {"Variables": {
                    "TWILIO_ACCOUNT_SID": ref("TwilioAccount"), "TWILIO_AUTH_TOKEN": ref("TwilioAuth"),
                    "TWILIO_API_KEY_SID": ref("TwilioKeySid"), "TWILIO_API_KEY_SECRET": ref("TwilioKeySecret"),
                    "BUCKET": "live-ninja-deliverables-759775734231", "TABLE": ref("Table"),
                    "PUBLIC_BASE_URL": sub("https://${Api}.execute-api.${AWS::Region}.amazonaws.com"),
                    "RUN_ID": "hotel-trial-2026-10-15",
                }},
            }},
            "Integration": {"Type": "AWS::ApiGatewayV2::Integration", "Properties": {"ApiId": ref("Api"), "IntegrationType": "AWS_PROXY", "IntegrationUri": {"Fn::GetAtt": ["Function", "Arn"]}, "PayloadFormatVersion": "2.0"}},
            "Route": {"Type": "AWS::ApiGatewayV2::Route", "Properties": {"ApiId": ref("Api"), "RouteKey": "$default", "Target": {"Fn::Join": ["/", ["integrations", ref("Integration")]]}}},
            "Stage": {"Type": "AWS::ApiGatewayV2::Stage", "Properties": {"ApiId": ref("Api"), "StageName": "$default", "AutoDeploy": True}},
            "Permission": {"Type": "AWS::Lambda::Permission", "Properties": {"FunctionName": ref("Function"), "Action": "lambda:InvokeFunction", "Principal": "apigateway.amazonaws.com", "SourceArn": sub("arn:${AWS::Partition}:execute-api:${AWS::Region}:${AWS::AccountId}:${Api}/*")}},
        },
        "Outputs": {"ServiceUrl": {"Value": sub("https://${Api}.execute-api.${AWS::Region}.amazonaws.com")}},
    }


def main():
    if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("GITHUB_REF") != "refs/heads/main":
        raise SystemExit("Deployment runs only in GitHub Actions on main.")
    sts = boto3.client("sts")
    if sts.get_caller_identity()["Account"] != "759775734231":
        raise SystemExit("Wrong AWS account.")
    cf = boto3.client("cloudformation")
    # The repository's legacy CLOUDFORMATION_S3_BUCKET is in us-east-2.
    # Reuse the same us-east-1 SAM artifact bucket as the main deployment.
    bootstrap = cf.describe_stacks(StackName="aws-sam-cli-managed-default")["Stacks"][0]
    bucket = next(o["OutputValue"] for o in bootstrap["Outputs"] if o["OutputKey"] == "SourceBucket")
    location = boto3.client("s3").get_bucket_location(Bucket=bucket).get("LocationConstraint") or "us-east-1"
    if location != os.environ.get("AWS_REGION", "us-east-1"):
        raise SystemExit("Lambda artifact bucket is in the wrong region.")
    key = "phone-trial/" + os.environ["GITHUB_SHA"] + "/recorder.zip"
    archive = io.BytesIO()
    with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as z:
        z.write(Path(__file__).with_name("recorder.py"), "recorder.py")
        z.write(Path(__file__).with_name("owner_messages.py"), "owner_messages.py")
    boto3.client("s3").put_object(Bucket=bucket, Key=key, Body=archive.getvalue(), ServerSideEncryption="AES256")
    params = {"CodeBucket": bucket, "CodeKey": key,
              "TwilioAccount": os.environ["TWILIO_ACCOUNT_SID"], "TwilioKeySid": os.environ["TWILIO_API_KEY_SID"],
              "TwilioAuth": os.environ["TWILIO_AUTH_TOKEN"], "TwilioKeySecret": os.environ["TWILIO_API_KEY_SECRET"]}
    name = "live-ninja-phone-trial"
    kwargs = dict(StackName=name, TemplateBody=json.dumps(template()),
                  Parameters=[{"ParameterKey": k, "ParameterValue": v} for k, v in params.items()],
                  Capabilities=["CAPABILITY_NAMED_IAM"],
                  Tags=[{"Key": k, "Value": v} for k, v in {"Project": "live-ninja", "CostCenter": "voice-ai", "Environment": "prod", "ManagedBy": "sam", "DeployedVia": "github-actions", "Owner": "jeremy"}.items()])
    try:
        current = cf.describe_stacks(StackName=name)["Stacks"][0]
        exists = True
    except ClientError as exc:
        if "does not exist" not in str(exc):
            raise SystemExit("Could not inspect recorder stack.")
        exists = False
    if exists and current["StackStatus"] == "ROLLBACK_COMPLETE":
        retained = [r for r in cf.describe_stack_resources(StackName=name)["StackResources"]
                    if r.get("ResourceStatus") == "DELETE_SKIPPED"]
        if any(r.get("ResourceType") != "AWS::DynamoDB::Table" or r.get("PhysicalResourceId") != "live-ninja-phone-trial" for r in retained):
            raise SystemExit("Unexpected retained resource; refusing failed-stack cleanup.")
        ddb = boto3.client("dynamodb")
        for resource in retained:
            # Deployment-only empty-table check, never a serving-path Scan.
            check = ddb.scan(TableName=resource["PhysicalResourceId"], Limit=1, Select="COUNT", ConsistentRead=True)
            if check.get("Count") or check.get("LastEvaluatedKey"):
                raise SystemExit("Retained trial table contains data; refusing cleanup.")
        cf.delete_stack(StackName=name)
        cf.get_waiter("stack_delete_complete").wait(StackName=name)
        for resource in retained:
            ddb.delete_table(TableName=resource["PhysicalResourceId"])
            ddb.get_waiter("table_not_exists").wait(TableName=resource["PhysicalResourceId"])
        exists = False
    try:
        if exists:
            cf.update_stack(**kwargs)
            cf.get_waiter("stack_update_complete").wait(StackName=name)
        else:
            cf.create_stack(**kwargs)
            cf.get_waiter("stack_create_complete").wait(StackName=name)
    except ClientError as exc:
        if "No updates are to be performed" not in str(exc):
            raise SystemExit("Recorder stack deployment failed; inspect CloudFormation events (never secret parameters).")
    outputs = cf.describe_stacks(StackName=name)["Stacks"][0]["Outputs"]
    url = next(o["OutputValue"] for o in outputs if o["OutputKey"] == "ServiceUrl")
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as out:
        out.write("recorder_url=" + url + "\n")
    print("Recorder stack ready.")


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        raise SystemExit("Recorder deployment failed: " + type(exc).__name__)
