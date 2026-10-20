#!/usr/bin/env ruby
require 'yaml'
require 'open3'

def render(*args)
  out, err, status = Open3.capture3('helm', 'template', 'test', 'charts/mailvault', *args)
  raise err unless status.success?
  YAML.load_stream(out).compact
end

def check(value, message)
  raise message unless value
end

def cron(docs)
  docs.find { |d| d['kind'] == 'CronJob' }['spec']
end

base = render
check(base.count { |d| d['kind'] == 'PersistentVolumeClaim' } == 2, 'base requires two PVCs')
check(base.none? { |d| %w[Deployment Service Pod NetworkPolicy].include?(d['kind']) }, 'Proton must be optional')
check(cron(base)['suspend'] == true, 'schedule must default to suspended')
check(cron(base)['concurrencyPolicy'] == 'Forbid', 'scheduled jobs must not overlap')
check(base.select { |d| d['kind'] == 'PersistentVolumeClaim' }.all? { |d| d['metadata']['annotations']['helm.sh/resource-policy'] == 'keep' }, 'PVCs must be retained')
base_image = cron(base)['jobTemplate']['spec']['template']['spec']['containers'][0]['image']
app_version = YAML.load_file('charts/mailvault/Chart.yaml')['appVersion']
check(app_version.match?(/\A(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?\z/), 'appVersion must be a semantic version')
check(base_image == "ghcr.io/zekihan/mailvault:#{app_version}", 'image must default to chart appVersion')
override = render('--set', 'image.tag=custom')
check(cron(override)['jobTemplate']['spec']['template']['spec']['containers'][0]['image'] == 'ghcr.io/zekihan/mailvault:custom', 'explicit image tag must take precedence')
setup = render('--set', 'proton.enabled=true')
check(setup.find { |d| d['kind'] == 'Deployment' }['spec']['replicas'] == 0, 'setup must stop Bridge')
bridge_setup = setup.find { |d| d['kind'] == 'Deployment' && d['metadata']['name'].end_with?('-bridge-setup') }
check(bridge_setup && bridge_setup['spec']['strategy']['type'] == 'Recreate', 'Bridge setup must use a replaceable Deployment')
check(setup.none? { |d| d['kind'] == 'Pod' }, 'immutable standalone setup Pod must not return')
changed = render('--set', 'proton.enabled=true,proton.tlsSecret=rotated-tls,proton.persistence.existingClaim=replacement-claim')
changed_setup = changed.find { |d| d['metadata']['name'].end_with?('-bridge-setup') }
check(changed_setup['spec']['selector'] == bridge_setup['spec']['selector'], 'setup selector must remain stable during upgrades')
changed_volumes = changed_setup['spec']['template']['spec']['volumes']
check(changed_volumes.any? { |v| v.dig('secret', 'secretName') == 'rotated-tls' }, 'TLS changes must reach the replaceable Pod template')
check(changed_volumes.any? { |v| v.dig('persistentVolumeClaim', 'claimName') == 'replacement-claim' }, 'claim changes must reach the replaceable Pod template')
running = render('--set', 'proton.enabled=true,proton.setup=false,initialized=true,suspend=false')
check(running.none? { |d| d['kind'] == 'Pod' }, 'setup Pod must be removed for runtime')
deployment = running.find { |d| d['kind'] == 'Deployment' }['spec']
check(deployment['replicas'] == 1 && deployment['strategy']['type'] == 'Recreate', 'single Bridge writer required')
bridge = deployment['template']['spec']['containers'][0]
check(bridge['env'].include?({'name'=>'BRIDGE_LISTEN_ADDRESS', 'value'=>'0.0.0.0'}), 'direct listener must be enabled')
check(bridge['volumeMounts'].any? { |m| m['mountPath'] == '/tls' && m['readOnly'] }, 'imported certificate paths must persist at runtime')
pod = cron(running)['jobTemplate']['spec']['template']['spec']
check(pod['containers'].size == 1 && !pod.key?('initContainers'), 'no proxies or sidecars permitted')
ca = pod['volumes'].find { |v| v['name'] == 'bridge-ca' }
check(ca['secret']['items'] == [{'key'=>'ca.crt', 'path'=>'ca.crt'}], 'Mailvault must never mount the private TLS key')
check(running.find { |d| d['kind'] == 'Service' }['spec']['ports'].map { |p| p['port'] } == [1143], 'only IMAP is exposed')
check(running.any? { |d| d['kind'] == 'NetworkPolicy' }, 'Bridge ingress policy missing')
existing = render('--set', 'proton.enabled=true,persistence.state.existingClaim=state,persistence.backups.existingClaim=backups,proton.persistence.existingClaim=bridge')
check(existing.none? { |d| d['kind'] == 'PersistentVolumeClaim' }, 'existing claims must not be recreated')
['setup.enabled=true,initialized=true,suspend=false', 'suspend=false', 'proton.enabled=true,proton.setup=true,initialized=true,suspend=false'].each do |values|
  _, _, status = Open3.capture3('helm', 'template', 'test', 'charts/mailvault', '--set', values)
  check(!status.success?, "unsafe configuration accepted: #{values}")
end
puts 'Helm lifecycle, persistence, TLS isolation and optional Proton checks passed'

[false, true].each do |proton|
  docs = render('--set', "setup.enabled=true,proton.enabled=#{proton},proton.setup=false")
  setup_spec = docs.find { |d| d['metadata']['name'] == 'test-mailvault-setup' }['spec']
  check(setup_spec['strategy']['type'] == 'Recreate', 'OAuth setup must use one replaceable writer')
  setup_pod = setup_spec['template']['spec']
  job_pod = cron(docs)['jobTemplate']['spec']['template']['spec']
  check(setup_pod['volumes'] == job_pod['volumes'], 'OAuth setup must reuse job state, backup, config and CA volumes')
  setup_container = setup_pod['containers'][0].dup
  check(setup_container.delete('command') == ['sleep', 'infinity'], 'setup must wait for interactive auth, not run sync')
  job_container = job_pod['containers'][0].dup
  job_container.delete('args')
  check(setup_container == job_container, 'OAuth setup must match runtime image, mounts, trust and security settings')
end
%w[secrets/proton-tls/ca.key secrets/proton-tls/tls.key proton-tls/ca.key proton-tls/tls.key].each do |path|
  _, _, status = Open3.capture3('git', 'check-ignore', '--no-index', path)
  check(status.success?, "private key path must be ignored: #{path}")
end
readme = File.read('charts/mailvault/README.md')
check(readme.include?('mkdir -m 700 secrets/proton-tls'), 'TLS instructions must use ignored storage')
check(readme.include?('auth login --source hotmail') && readme.include?('oauth2.token_file: /state/SOURCE-oauth.json'), 'OAuth initialization instructions missing')
puts 'Setup upgrades, OAuth lifecycle and private-key ignore regressions passed'
