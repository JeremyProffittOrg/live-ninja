import unittest
from metric_release import validate_changes
class GuardTests(unittest.TestCase):
 def test_rejects_configuration_or_delete(self):
  for prop in ['Environment','Role','VpcConfig']:
   change={'Changes':[{'ResourceChange':{'LogicalResourceId':'F','ResourceType':'AWS::Lambda::Function','Action':'Modify','Replacement':'False','Scope':['Properties'],'Details':[{'Target':{'Name':prop,'RequiresRecreation':'Never'}}]}}]}
   with self.assertRaises(ValueError):validate_changes(change,{'F':{}},[])
 def test_allows_only_code(self):
  change={'Changes':[{'ResourceChange':{'LogicalResourceId':'F','ResourceType':'AWS::Lambda::Function','Action':'Modify','Replacement':'False','Scope':['Properties'],'Details':[{'Target':{'Name':'Code','RequiresRecreation':'Never'}}]}}]}
  validate_changes(change,{'F':{}},[])
  change['Changes'][0]['ResourceChange']['Action']='Remove'
  with self.assertRaises(ValueError):validate_changes(change,{'F':{}},[])

from original_template_patch import patch_original
class OriginalTemplateTests(unittest.TestCase):
 def test_preserves_unrelated_literal_and_intrinsic(self):
  import yaml
  from original_template_patch import Loader
  source="""Transform: AWS::Serverless-2016-10-31
Resources:
  F:
    Type: AWS::Serverless::Function
    Properties:
      CodeUri:
        Bucket: old-bucket
        Key: old.zip
      Description: |-
        Literal multiline description.
      Role: !GetAtt Role.Arn
  Keep:
    Type: AWS::S3::Bucket
"""
  after=patch_original(source,{'F':{'S3Bucket':'bucket','S3Key':'new.zip'}},[])
  self.assertIn('Description: |-\n        Literal multiline description.',after)
  self.assertIn('Role: !GetAtt Role.Arn',after)
  self.assertEqual(yaml.load(after,Loader=Loader)['Resources']['F']['Properties']['CodeUri'],{'Bucket':'bucket','Key':'new.zip'})
 def test_filter_only(self):
  source="""Resources:
  Filter:
    Type: AWS::Logs::MetricFilter
    Properties:
      FilterPattern: '{}'
      MetricTransformations: [{MetricName: Heartbeat, MetricNamespace: Example, MetricValue: '1'}]
"""
  result=patch_original(source,{},['Filter'])
  self.assertIn('__operational_custom_metrics_enabled',result)
  self.assertIn('MetricName: Heartbeat',result)
