"""Manual metric-only release. No configuration, identity, schedule or IAM changes."""
import copy,json,os,pathlib,subprocess,sys,zipfile
def validate_changes(change, functions, filters):
 if change.get('NextToken'):raise ValueError('Incomplete change set')
 seen=set()
 for c in change.get('Changes',[]):
  r=c['ResourceChange'];name=r['LogicalResourceId'];seen.add(name)
  if r['Action']!='Modify' or r['Replacement']!='False' or r.get('Scope')!=['Properties']:raise ValueError('Unapproved mutation')
  allowed={'Code'} if name in functions else {'FilterPattern','MetricTransformations'} if name in filters else set()
  # Code references can cause an unchanged API body to be reevaluated.
  if r['ResourceType'] in ('AWS::ApiGatewayV2::Api','AWS::ApiGateway::RestApi'):
   allowed={'Body'}
   if any(d.get('ChangeSource')!='ResourceAttribute' or d.get('CausingEntity','').split('.')[0] not in functions for d in r.get('Details',[])):raise ValueError('Unrelated API change')
  if not allowed or not r.get('Details'):raise ValueError('Unapproved resource')
  for d in r['Details']:
   t=d['Target']
   if t.get('Name') not in allowed or t.get('RequiresRecreation')!='Never':raise ValueError('Unapproved property')
 if not (set(functions)|set(filters)).issubset(seen):raise ValueError('Incomplete intended update')

def main():
 cfg=json.loads(pathlib.Path('scripts/metric-release.json').read_text())
 if os.environ.get('GITHUB_ACTIONS')!='true' or os.environ.get('GITHUB_EVENT_NAME')!='workflow_dispatch' or os.environ.get('GITHUB_REF')!='refs/heads/main' or os.environ.get('GITHUB_REPOSITORY')!=cfg['repo']:raise ValueError('Manual main release required')
 import boto3
 s=boto3.Session(region_name='us-east-1')
 if s.client('sts').get_caller_identity()['Account']!='759775734231':raise ValueError('Wrong account')
 cf=s.client('cloudformation');current=cf.describe_stacks(StackName=cfg['stack'])['Stacks'][0]
 if current['StackStatus'] not in ('CREATE_COMPLETE','UPDATE_COMPLETE'):raise ValueError('Stack not stable')
 live=cf.get_template(StackName=cfg['stack'],TemplateStage='Processed')['TemplateBody']
 if isinstance(live,str):live=json.loads(live)
 if live.get('Transform'):raise ValueError('Processed template required')
 new=copy.deepcopy(live);sha=os.environ['GITHUB_SHA'];prefix='metric-release/'+sha+'/';bucket=cfg['bucket'];out=pathlib.Path('metric-build');out.mkdir(exist_ok=True)
 s.client('s3').head_bucket(Bucket=bucket,ExpectedBucketOwner='759775734231')
 for logical,entry in cfg['functions'].items():
  p=new['Resources'][logical]['Properties']
  if new['Resources'][logical]['Type']!='AWS::Lambda::Function' or p.get('PackageType','Zip')!='Zip':raise ValueError('Not expected zip Lambda')
  if not isinstance(p.get('Architectures'),list) or len(p['Architectures'])!=1:raise ValueError('Architecture unknown')
  arch={'arm64':'arm64','x86_64':'amd64'}[p['Architectures'][0]]
  env=os.environ.copy();env.update(GOOS='linux',GOARCH=arch,CGO_ENABLED='0')
  dest=(out/logical).resolve();dest.mkdir(exist_ok=True)
  print('Building reviewed publisher:',logical,flush=True)
  flags=['-tags','lambda.norpc','-trimpath']
  if cfg['repo'].endswith('/live-ninja'):
   version=pathlib.Path('VERSION').read_text().strip()+'+'+sha[:8]
   flags+=['-ldflags','-s -w -X github.com/JeremyProffittOrg/live-ninja/internal/webapp.BuildVersion='+version]
  subprocess.run(['go','build',*flags,'-o',str(dest/'bootstrap'),entry['package']],cwd=entry['cwd'],env=env,check=True)
  zi=zipfile.ZipInfo('bootstrap');zi.create_system=3;zi.external_attr=0o100755<<16;zi.compress_type=zipfile.ZIP_DEFLATED
  with zipfile.ZipFile(dest/'function.zip','w') as z:z.writestr(zi,(dest/'bootstrap').read_bytes())
  key=prefix+logical+'.zip';s.client('s3').put_object(Bucket=bucket,Key=key,Body=(dest/'function.zip').read_bytes(),ServerSideEncryption='AES256')
  p['Code']={'S3Bucket':bucket,'S3Key':key}
 for logical in cfg['filters']:
  r=new['Resources'][logical]
  if r['Type']!='AWS::Logs::MetricFilter':raise ValueError('Unexpected filter')
  r['Properties']['FilterPattern']='{ $.__operational_custom_metrics_enabled = true }'
  for transform in r['Properties']['MetricTransformations']:transform.pop('DefaultValue',None)
 key=prefix+'template.json';s.client('s3').put_object(Bucket=bucket,Key=key,Body=json.dumps(new).encode(),ServerSideEncryption='AES256')
 name='metrics-'+os.environ['GITHUB_RUN_ID']+'-'+os.environ['GITHUB_RUN_ATTEMPT']
 cf.create_change_set(StackName=cfg['stack'],ChangeSetName=name,ChangeSetType='UPDATE',TemplateURL='https://'+bucket+'.s3.us-east-1.amazonaws.com/'+key,Capabilities=['CAPABILITY_IAM','CAPABILITY_NAMED_IAM'],Tags=current.get('Tags',[]),Parameters=[{'ParameterKey':p['ParameterKey'],'UsePreviousValue':True} for p in current.get('Parameters',[])])
 cf.get_waiter('change_set_create_complete').wait(StackName=cfg['stack'],ChangeSetName=name)
 change=cf.describe_change_set(StackName=cfg['stack'],ChangeSetName=name);validate_changes(change,cfg['functions'],cfg['filters'])
 print('Validated metric-only changes:',','.join(cfg['functions'])+','.join(cfg['filters']),flush=True)
 cf.execute_change_set(StackName=cfg['stack'],ChangeSetName=name);cf.get_waiter('stack_update_complete').wait(StackName=cfg['stack'])
 print('Metric-only release complete. Verify subsequent publication independently.',flush=True)
if __name__=='__main__':
 try:main()
 except Exception as error:
  code=getattr(error,'response',{}).get('Error',{}).get('Code',type(error).__name__)
  print('Metric release halted ('+code+'); inspect nonsecret stack/run state. No permission expansion or automatic retry.',file=sys.stderr);sys.exit(1)
