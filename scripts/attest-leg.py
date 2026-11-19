#!/usr/bin/env python3
# Copyright (c) 2026 The Ycash developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or https://www.opensource.org/licenses/mit-license.php .

"""The light attestor's regtest leg (beta4 item 3d, B-T1; workspace docs/plans/beta4/price-feed.md
section 2.5 B-2, B-3): SubmitAttestation through this server, on a ycash6 node and on a kogany node.

    scripts/attest-leg.py --ycashd Y [--koganyd Z] [--dir D] [--port-base 39100] [--keep]

The chain is built with the kogany rig's pieces (scripts/kogany-rig.py, imported, not changed): a
ycash6 `ycashd` with a wallet that mines, and, with --koganyd, a kogany node the ycashd dials
(`connect=`, so every block reaches kogany over P2P and kogany validates it). Steps:

  1. ycashd alone mines to the Vault activation (110) + 2 and creates the YED attestor set
     (`set_create`: open, maturity 8, the regtest bond floor; the devnet's spec);
  2. kogany starts with that set id; ycashd restarts with it and `connect=` kogany; kogany syncs;
  3. ycashd's wallet registers three attestors (`yed_registerattestor 10 0`, one per block), mines
     through BOND_MATURITY and ATTEST_ARM_DELAY, so the set is seated (ARMED on ycashd);
  4. one lightwalletd-dd (`--yellowback`) on each node;
  5. `go test -tags devnet -run TestAttestLeg ./frontend/` with LWD_ATTEST_SERVERS naming the
     servers: the statements signed by ycashd's wallet (`yed_signattestation`) go through
     SubmitAttestation and must appear in GetAttestations and in BuildBundle on each node.

Ports: --port-base .. +8 (kogany p2p, v2, rpc; ycashd p2p, rpc; two servers, gRPC and HTTP). Never
mainnet; scratch directories only (--dir, default a temporary one). Exit status is go test's.
"""

import argparse
import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
_spec = importlib.util.spec_from_file_location('kogany_rig', os.path.join(ROOT, 'scripts', 'kogany-rig.py'))
rig = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(rig)

VAULT = 110
SET_SPEC = {'seats': 15, 'unlockthreshold': 1, 'cancelthreshold': 1, 'slashthreshold': 1, 'open': True,
            'maturity': 8, 'bondmin': 10, 'bondlockmin': 0, 'livenesswindow': 100000}
ATTESTORS = 3          # ATTEST_ARM_MIN on regtest
BOND_YEC = 10
BOND_MATURITY = 8
ATTEST_ARM_DELAY = 8


def ycash_conf(ydir, ports, connect):
    with open(os.path.join(ydir, 'ycash.conf'), 'w', encoding='utf-8') as f:
        f.write('regtest=1\nserver=1\nshowmetrics=0\nprinttoconsole=0\nrpcuser=rig\nrpcpassword=rig\n'
                'rpcallowip=127.0.0.1\nlistenonion=0\ndiscover=0\ndnsseed=0\ntxindex=1\n'
                'experimentalfeatures=1\ninsightexplorer=1\nlisten=0\n'
                'port=%d\nrpcport=%d\n' % (ports['yp2p'], ports['yrpc']))
        if connect:
            f.write('connect=127.0.0.1:%d\n' % ports['kp2p'])


def start_ycashd(binary, workdir, ports, setid, connect):
    ydir = os.path.join(workdir, 'ycashd')
    os.makedirs(ydir, exist_ok=True)
    ycash_conf(ydir, ports, connect)
    argv = [binary, '-datadir=' + ydir, '-i-am-aware-zcashd-will-be-replaced-by-zebrad-and-zallet-in-2025']
    argv += ['-nuparams=%s:1' % b for b in rig.NUPARAMS]
    argv += ['-nuparams=%s:%d' % (rig.VAULT_BRANCH, VAULT)]
    if setid:
        argv += ['-yellowbackattestorset=' + setid]
    p = rig.spawn(argv, os.path.join(workdir, 'ycashd.log'))
    node = rig.Node({'url': 'http://127.0.0.1:%d/' % ports['yrpc'], 'user': 'rig', 'password': 'rig'})
    rig.wait_for('ycashd RPC', lambda: node('getblockcount', timeout=5) is not None, 300)
    return p, node


def stop(p, node=None):
    if node is not None:
        try:
            node('stop', timeout=10)
        except Exception:  # noqa: BLE001 - it is going away
            pass
    try:
        p.wait(timeout=60)
    except subprocess.TimeoutExpired:
        p.kill()
        p.wait()


def mine(state, node, n, kogany):
    if kogany:
        return rig.mine(state, n)
    node('generate', n, timeout=120 + 10 * n)
    return node('getblockcount')


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--ycashd', required=True)
    ap.add_argument('--koganyd')
    ap.add_argument('--dir')
    ap.add_argument('--port-base', type=int, default=39100)
    ap.add_argument('--lightwalletd', help='a built lightwalletd-dd (default: build one into DIR)')
    ap.add_argument('--keep', action='store_true', help='leave everything running (stop with kogany-rig.py --dir D down)')
    ap.add_argument('--run', default='TestAttestLeg')
    args = ap.parse_args(argv)

    workdir = os.path.abspath(args.dir or os.path.join(tempfile.mkdtemp(prefix='attest-leg-'), 'rig'))
    shutil.rmtree(workdir, ignore_errors=True)
    os.makedirs(workdir)
    ports = {k: args.port_base + i for i, k in enumerate(rig.PORT_NAMES)}
    lwd = args.lightwalletd
    if not lwd:
        lwd = os.path.join(workdir, 'bin', 'lightwalletd')
        subprocess.check_call(['go', 'build', '-mod=vendor', '-o', lwd, '.'], cwd=ROOT, env=dict(os.environ, CGO_ENABLED='0'))
    kogany_on = bool(args.koganyd)
    state = {'dir': workdir, 'num_nodes': 2 if kogany_on else 1, 'pools': [0], 'vault_height': VAULT,
             'ports': ports, 'pids': {}, 'rpc': {
                 '0': {'url': 'http://127.0.0.1:%d/' % ports['yrpc'], 'port': ports['yrpc'], 'user': 'rig',
                       'password': 'rig', 'role': 'sidecar'}}}
    if kogany_on:
        state['rpc']['1'] = {'url': 'http://127.0.0.1:%d/' % ports['krpc'], 'port': ports['krpc'], 'user': 'kogany',
                             'password': 'kogany', 'role': 'kogany'}
    rig.save(state)
    status = 1
    try:
        print('[1] ycashd alone: mine to the Vault activation, create the YED attestor set', flush=True)
        y, node = start_ycashd(args.ycashd, workdir, ports, None, False)
        state['pids']['ycashd'] = y.pid
        rig.save(state)
        node('generate', VAULT + 2, timeout=600)
        created = node('set_create', dict(SET_SPEC))
        node('generate', 1)
        setid = created['setid']
        state['attestor_set'] = setid
        stop(y, node)

        if kogany_on:
            print('[2] kogany with set %s; ycashd restarts with it, connect= kogany' % setid, flush=True)
            z, kogany = rig.start_koganyd(args.koganyd, workdir, ports, [], VAULT, setid)
            state['pids']['koganyd'] = z.pid
            rig.save(state)
        else:
            print('[2] ycashd restarts with set %s (no kogany)' % setid, flush=True)
        y, node = start_ycashd(args.ycashd, workdir, ports, setid, kogany_on)
        state['pids']['ycashd'] = y.pid
        rig.save(state)
        if kogany_on:
            rig.wait_for('ycashd connected to kogany', lambda: node('getpeerinfo'), 60)
            tip = node('getbestblockhash')
            rig.wait_for('kogany synced to %s' % tip, lambda: kogany('getbestblockhash') == tip, 300)

        print('[3] register %d attestors from ycashd\'s wallet, then BOND_MATURITY and ATTEST_ARM_DELAY' % ATTESTORS, flush=True)
        keys = []
        for _ in range(ATTESTORS):
            res = node('yed_registerattestor', BOND_YEC, 0)
            keys.append(res['attestorPubKey'])
            mine(state, node, 1, kogany_on)
        mine(state, node, BOND_MATURITY, kogany_on)
        mine(state, node, ATTEST_ARM_DELAY + 2, kogany_on)
        rows = node('yed_listattestors')
        seqs = [r['seq'] for r in rows if r['attestorPubKey'] in keys]
        info = node('yed_getinfo')['attest']
        print('    seqs %s, attest %s (seated %s) at height %d' % (seqs, info.get('status'), info.get('seatedCount'),
                                                                  node('getblockcount')), flush=True)
        if len(seqs) != ATTESTORS:
            raise rig.RigError('registered %d attestors, yed_listattestors shows %s' % (ATTESTORS, rows))
        if kogany_on:
            krows = kogany('yed_listattestors')
            print('    kogany: seqs %s, attest %s' % ([r['seq'] for r in krows],
                                                     kogany('yed_getinfo')['attest'].get('status')), flush=True)

        print('[4] lightwalletd-dd on each node', flush=True)
        servers = []
        p = rig.start_lightwalletd(lwd, workdir, 'lightwalletd-sidecar', ports['yrpc'], 'rig', 'rig', ports['lwd2'], ports['lwd2http'])
        state['pids']['lightwalletd-sidecar'] = p.pid
        servers.append('ycash6=127.0.0.1:%d' % ports['lwd2'])
        if kogany_on:
            p = rig.start_lightwalletd(lwd, workdir, 'lightwalletd', ports['krpc'], 'kogany', 'kogany', ports['lwd'], ports['lwdhttp'])
            state['pids']['lightwalletd'] = p.pid
            servers.append('kogany=127.0.0.1:%d' % ports['lwd'])
        rig.save(state)

        print('[5] go test -tags devnet -run %s (servers %s)' % (args.run, ', '.join(servers)), flush=True)
        first = servers[0].split('=', 1)[1]
        last = servers[-1].split('=', 1)[1]
        env = dict(os.environ, LWD_ATTEST_LEG='1', LWD_DEVNET_DIR=workdir, LWD_FORK_ADDR=last, LWD_BASELINE_ADDR=first,
                   LWD_ATTEST_SERVERS=','.join(servers), LWD_ATTEST_SEQS=','.join(str(s) for s in seqs))
        status = subprocess.call(['go', 'test', '-mod=vendor', '-tags', 'devnet', '-count=1', '-timeout', '600s', '-v',
                                  '-run', args.run, './frontend/'], cwd=ROOT, env=env)
    finally:
        if not args.keep:
            rig.down(state)
    print('attest-leg: %s (dir %s)' % ('PASS' if status == 0 else 'FAIL', workdir))
    return status


if __name__ == '__main__':
    try:
        sys.exit(main())
    except rig.RigError as e:
        print('attest-leg: %s' % e, file=sys.stderr)
        sys.exit(2)
