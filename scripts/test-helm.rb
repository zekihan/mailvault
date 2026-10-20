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
check(base_image == 'ghcr.io/zekihan/mailvault:dev', 'image must default to chart appVersion')
override = render('--set', 'image.tag=custom')
check(cron(override)['jobTemplate']['spec']['template']['spec']['containers'][0]['image'] == 'ghcr.io/zekihan/mailvault:custom', 'explicit image tag must take precedence')
setup = render('--set', 'proton.enabled=true')
check(setup.find { |d| d['kind'] == 'Deployment' }['spec']['replicas'] == 0, 'setup must stop Bridge')
check(setup.any? { |d| d['kind'] == 'Pod' }, 'setup pod missing')
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
['suspend=false', 'proton.enabled=true,proton.setup=true,initialized=true,suspend=false'].each do |values|
  _, _, status = Open3.capture3('helm', 'template', 'test', 'charts/mailvault', '--set', values)
  check(!status.success?, "unsafe configuration accepted: #{values}")
end
puts 'Helm lifecycle, persistence, TLS isolation and optional Proton checks passed'
