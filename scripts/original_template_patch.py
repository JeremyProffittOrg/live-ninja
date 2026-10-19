"""Patch only selected value spans in the deployed ORIGINAL CloudFormation YAML."""
import copy,json,yaml
from yaml.nodes import MappingNode

class Loader(yaml.SafeLoader): pass
def intrinsic(loader, tag, node):
 if isinstance(node,yaml.ScalarNode): value=loader.construct_scalar(node)
 elif isinstance(node,yaml.SequenceNode):value=loader.construct_sequence(node)
 else:value=loader.construct_mapping(node)
 return {'!'+tag:value}
Loader.add_multi_constructor('!',intrinsic)
def member(node,key):
 if not isinstance(node,MappingNode):raise ValueError('Expected mapping')
 for k,v in node.value:
  if k.value==key:return v
 raise ValueError('Missing exact source property: '+key)
def patch_original(text, codes, filters):
 if not isinstance(text,str):raise ValueError('Original YAML source required')
 tree=yaml.compose(text);before=yaml.load(text,Loader=Loader);expected=copy.deepcopy(before);edits=[]
 for name,code in codes.items():
  resource=expected['Resources'][name];kind=resource['Type']
  prop='CodeUri' if kind=='AWS::Serverless::Function' else 'Code' if kind=='AWS::Lambda::Function' else None
  if not prop:raise ValueError('Unexpected function kind')
  value={'Bucket':code['S3Bucket'],'Key':code['S3Key']} if prop=='CodeUri' else code
  node=member(member(member(member(tree,'Resources'),name),'Properties'),prop)
  replacement=json.dumps(value)
  if node.end_mark.line>node.start_mark.line:replacement+='\n'+' '*node.end_mark.column
  edits.append((node.start_mark.index,node.end_mark.index,replacement));resource['Properties'][prop]=value
 for name in filters:
  resource=expected['Resources'][name]
  if resource['Type']!='AWS::Logs::MetricFilter':raise ValueError('Unexpected filter')
  if any('DefaultValue' in x for x in resource['Properties']['MetricTransformations']):raise ValueError('Zero-datapoint default needs separate exact patch')
  node=member(member(member(member(tree,'Resources'),name),'Properties'),'FilterPattern')
  value='{ $.__operational_custom_metrics_enabled = true }'
  edits.append((node.start_mark.index,node.end_mark.index,json.dumps(value)));resource['Properties']['FilterPattern']=value
 for start,end,value in sorted(edits,reverse=True):text=text[:start]+value+text[end:]
 if yaml.load(text,Loader=Loader)!=expected:raise ValueError('Unexpected original-template semantic change')
 return text
