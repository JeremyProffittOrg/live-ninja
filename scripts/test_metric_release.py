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
