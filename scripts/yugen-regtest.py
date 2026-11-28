#!/usr/bin/env python3
# Copyright (c) 2026 The Ycash developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or https://www.opensource.org/licenses/mit-license.php .
"""Yugen compatibility on regtest: lightwalletd against a Yugen-activated ycashd.

One ycashd (Vault at 16, Yugen at 32, -yugenteststubproofs: TEST-ONLY bundle proofs) gets a Yugen
leaf transaction (a deposit by yewd-yugen), a bundle (submitbundle, its proofs in the block's
sidecar) and ordinary transactions; then this server (--yellowback) syncs the chain and the
script checks: no block skipped or refused, GetLatestBlock, GetBlock/GetBlockRange (Yugen
transactions omitted from compact blocks: they have no Sapling parts), GetTransaction of a Yugen
tx, SendTransaction of an ordinary tx, the YellowbackStreamer and vault read calls, rpcversion 12.

  scripts/yugen-regtest.py   (env: YCASHD, YUGEN_LEAFVERIFY, YEWD_YUGEN, LWD_BIN, YUGEN_RT_PORT=30400)

Needs grpcurl and go. Exit status 0 = every check passed.
"""
import base64, json, os, shutil, struct, subprocess, sys, tempfile, time, urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CLONES = os.path.dirname(ROOT)
YCASHD = os.environ.get('YCASHD', os.path.join(CLONES, 'ycash6', 'src', 'ycashd'))
HELPER = os.environ.get('YUGEN_LEAFVERIFY', os.path.join(
    CLONES, 'ycash6', 'contrib', 'yugen-leafverify', 'target', 'release', 'yugen-leafverify'))
WALLET = os.environ.get('YEWD_YUGEN', os.path.join(CLONES, 'yew', 'yewd', 'yugen', 'target', 'release', 'yewd-yugen'))
PORT = int(os.environ.get('YUGEN_RT_PORT', '30400'))
RPC, P2P, GRPC = PORT, PORT + 1, PORT + 2
ACT = 32
YUGEN_BRANCH_ID = 0x59474E30
ARGS = ['-regtest', '-nuparams=5ba81b19:1', '-nuparams=76b809bb:1', '-nuparams=374d694f:1',
        '-nuparams=8e471bd6:1', '-nuparams=66314da3:1', '-nuparams=19bd2d2f:1',
        '-nuparams=6d5b7a31:16', '-nuparams=59474e30:%d' % ACT, '-yugenverifier=' + HELPER,
        '-yugenteststubproofs', '-debug=yugen', '-listen=0', '-txindex', '-insightexplorer',
        '-experimentalfeatures', '-showmetrics=0']
FAIL = []


def check(cond, what):
    print(('ok    ' if cond else 'FAIL  ') + what, flush=True)
    if not cond:
        FAIL.append(what)


def rpc(method, *params):
    req = urllib.request.Request('http://127.0.0.1:%d' % RPC, json.dumps(
        {'jsonrpc': '1.0', 'id': 1, 'method': method, 'params': list(params)}).encode())
    req.add_header('Authorization', 'Basic ' + base64.b64encode(b'u:p').decode())
    try:
        r = json.loads(urllib.request.urlopen(req, timeout=600).read())
    except urllib.error.HTTPError as e:
        r = json.loads(e.read())
    if r.get('error'):
        raise RuntimeError('%s: %s' % (method, r['error']))
    return r['result']


def compact(n):
    return bytes([n]) if n < 253 else b'\xfd' + struct.pack('<H', n) if n < 0x10000 else b'\xfe' + struct.pack('<I', n)


def bundle_tx(bundle):
    return (struct.pack('<I', 4 | 0x80000000) + struct.pack('<I', 0x59474E47) + b'\x00\x00' + b'\x00' * 8 +
            b'\x00' * 8 + b'\x00\x00\x00' + compact(len(bundle)) + bundle + b'\x00\x00').hex()


def grpc(method, body=None, stream=False):
    cmd = ['grpcurl', '-plaintext', '-max-msg-sz', '67108864', '-import-path', os.path.join(ROOT, 'walletrpc'),
           '-proto', 'service.proto', '-proto', 'yellowback.proto']
    if body is not None:
        cmd += ['-d', json.dumps(body)]
    out = subprocess.run(cmd + ['127.0.0.1:%d' % GRPC, method], capture_output=True, text=True, timeout=120)
    if out.returncode != 0:
        raise RuntimeError('%s: %s' % (method, out.stderr.strip()))
    if stream:
        dec, s, objs = json.JSONDecoder(), out.stdout.strip(), []
        while s:
            o, i = dec.raw_decode(s)
            objs.append(o)
            s = s[i:].lstrip()
        return objs
    return json.loads(out.stdout or '{}')


def main():
    work = tempfile.mkdtemp(prefix='yugen-lwd-rt-')
    ndir = os.path.join(work, 'node')
    os.makedirs(ndir)
    with open(os.path.join(ndir, 'ycash.conf'), 'w') as f:
        f.write('regtest=1\nrpcuser=u\nrpcpassword=p\nrpcport=%d\nport=%d\n' % (RPC, P2P))
    def start(extra=()):
        n = subprocess.Popen([YCASHD, '-datadir=' + ndir] + ARGS + list(extra), stdout=subprocess.DEVNULL,
                             stderr=subprocess.DEVNULL)
        for _ in range(120):
            try:
                rpc('getblockcount')
                return n
            except Exception:
                time.sleep(1)
        raise RuntimeError('ycashd did not start')
    node = start()
    lwd = None
    try:
        rpc('generate', 110)
        rpc('sendtoaddress', rpc('getnewaddress'), 10)   # set_create spends no coinbase
        rpc('generate', 1)
        # The YED attestor set, so the node serves yed_* and the YellowbackStreamer starts.
        setid = rpc('set_create', {'seats': 15, 'unlockthreshold': 1, 'cancelthreshold': 1, 'slashthreshold': 1,
                                   'open': True, 'maturity': 2})['setid']
        for _ in range(60):
            if rpc('getrawmempool'):
                break
            time.sleep(0.5)
        rpc('generate', 1)
        rpc('stop')
        node.wait(timeout=120)
        node = start(['-yellowbackattestorset=' + setid])
        rpc('generate', 28)
        reg = run([HELPER, 'regroot'])
        payee = rpc('validateaddress', rpc('getnewaddress'))['scriptPubKey']

        # A Yugen leaf transaction: a deposit (shield) by the Yugen wallet engine.
        wdir = os.path.join(work, 'wallet')
        os.makedirs(wdir)
        wallet = lambda *a: json.loads(run([WALLET, '-network=regtest', '-node=127.0.0.1:%d' % RPC, '-rpcuser=u',
                                            '-rpcpassword=p', '-datadir=' + wdir, '-seed=' + '33' * 32] + list(a)))
        dep = wallet('depositaddress')
        dep = dep if isinstance(dep, str) else dep['address']
        fund = rpc('sendtoaddress', dep, 5)
        rpc('generate', 1)
        before = set(rpc('getrawmempool'))
        wallet('shield', json.dumps([fund]))
        leaf = [t for t in rpc('getrawmempool') if t not in before]
        check(len(leaf) == 1, 'yewd-yugen deposit is one Yugen leaf transaction in the mempool')
        leaf_block = rpc('generate', 1)[0]
        check(leaf[0] in rpc('getblock', leaf_block)['tx'], 'leaf transaction mined')

        # A bundle (stub proofs; the proofs travel in the block's sidecar).
        h = rpc('getblockcount') + 1
        ha = (h - 1) // 16 * 16
        a = rpc('yugen_getanchor', ha)
        req = {'anchor': {'slot': (ha // 16) & 0xff, 'anchor_root': a['root'], 'reg_root': reg,
                          'block_hash': a['block_hash']},
               'n': 5, 'seed': 1, 'payee': payee, 'branch_id': YUGEN_BRANCH_ID, 'network': 2, 'height': h,
               'activation': ACT}
        path = os.path.join(work, 'tb.json')
        with open(path, 'w') as f:
            json.dump(req, f)
        b, pa, pb = run([HELPER, 'testbundle', path]).split()
        bundle = rpc('submitbundle', {'tx': bundle_tx(bytes.fromhex(b)), 'proofs': [pa, pb]})['txid']
        bundle_block = rpc('generate', 1)[0]
        check(bundle in rpc('getblock', bundle_block)['tx'], 'bundle transaction mined')
        raw = rpc('getblock', bundle_block, 0)
        print('info  bundle block hex %d bytes' % (len(raw) // 2))
        # An ordinary transaction after activation, mined.
        rpc('sendtoaddress', rpc('getnewaddress'), 1)
        rpc('generate', 1)
        tip = rpc('getblockcount')

        # The server.
        lwd_bin = os.environ.get('LWD_BIN') or os.path.join(work, 'lightwalletd')
        if not os.environ.get('LWD_BIN'):
            subprocess.run(['go', 'build', '-mod=vendor', '-o', lwd_bin, '.'], cwd=ROOT, check=True)
        conf = os.path.join(ndir, 'ycash.conf')
        lwd_log = os.path.join(work, 'lightwalletd.log')
        os.makedirs(os.path.join(work, 'lwd'))
        lwd = subprocess.Popen([lwd_bin, '--zcash-conf-path', conf, '--no-tls-very-insecure', '--grpc-bind-addr',
                                '127.0.0.1:%d' % GRPC, '--http-bind-addr', '127.0.0.1:%d' % (PORT + 3),
                                '--data-dir', os.path.join(work, 'lwd'), '--log-file', lwd_log, '--yellowback'],
                               stdout=subprocess.DEVNULL, stderr=subprocess.STDOUT)
        latest = None
        for _ in range(90):
            try:
                latest = int(grpc('cash.z.wallet.sdk.rpc.CompactTxStreamer/GetLatestBlock').get('height', 0))
                blocks = grpc('cash.z.wallet.sdk.rpc.CompactTxStreamer/GetBlockRange',
                              {'start': {'height': tip}, 'end': {'height': tip}}, stream=True)
                if blocks:
                    break
            except Exception:
                pass
            time.sleep(1)
        check(latest == tip, 'GetLatestBlock = node tip %d (got %s)' % (tip, latest))
        rng = grpc('cash.z.wallet.sdk.rpc.CompactTxStreamer/GetBlockRange',
                   {'start': {'height': 1}, 'end': {'height': tip}}, stream=True)
        heights = [int(x['height']) for x in rng]
        check(heights == list(range(1, tip + 1)), 'GetBlockRange 1..%d: every block, none skipped' % tip)
        for name, bh, txid in (('leaf', leaf_block, leaf[0]), ('bundle', bundle_block, bundle)):
            hh = rpc('getblock', bh)['height']
            cb = grpc('cash.z.wallet.sdk.rpc.CompactTxStreamer/GetBlock', {'height': hh})
            ids = [base64.b64decode(t['hash'])[::-1].hex() for t in cb.get('vtx', [])]
            check(cb.get('hash') and base64.b64decode(cb['hash'])[::-1].hex() == bh,
                  'GetBlock %s block %d: hash matches' % (name, hh))
            check(txid not in ids, 'GetBlock %s block: Yugen tx omitted from the compact block' % name)
            rt = grpc('cash.z.wallet.sdk.rpc.CompactTxStreamer/GetTransaction',
                      {'hash': base64.b64encode(bytes.fromhex(txid)[::-1]).decode()})
            check(base64.b64decode(rt.get('data', '')).hex() == rpc('getrawtransaction', txid),
                  'GetTransaction %s tx: raw bytes as the node has them' % name)

        # SendTransaction of an ordinary transaction, post-activation.
        u = rpc('listunspent')[0]
        dst = rpc('getnewaddress')
        rawtx = rpc('createrawtransaction', [{'txid': u['txid'], 'vout': u['vout']}],
                    {dst: round(float(u['amount']) - 0.0001, 8)})
        signed = rpc('signrawtransaction', rawtx)['hex']
        sr = grpc('cash.z.wallet.sdk.rpc.CompactTxStreamer/SendTransaction',
                  {'data': base64.b64encode(bytes.fromhex(signed)).decode(), 'height': tip})
        check(int(sr.get('errorCode', 0)) == 0 and len(rpc('getrawmempool')) == 1,
              'SendTransaction of an ordinary tx: accepted (%s)' % sr.get('errorMessage', ''))
        rpc('generate', 1)
        for _ in range(30):
            if int(grpc('cash.z.wallet.sdk.rpc.CompactTxStreamer/GetLatestBlock').get('height', 0)) == tip + 1:
                break
            time.sleep(1)
        check(int(grpc('cash.z.wallet.sdk.rpc.CompactTxStreamer/GetLatestBlock').get('height', 0)) == tip + 1,
              'server follows the next block')

        # The YellowbackStreamer and vault read calls.
        for m in ('GetYellowbackInfo', 'GetActivation', 'GetChainInfo', 'GetVaultInfo', 'GetStats'):
            try:
                grpc('cash.z.wallet.sdk.rpc.YellowbackStreamer/' + m)
                check(True, 'YellowbackStreamer/%s' % m)
            except Exception as e:
                check(False, 'YellowbackStreamer/%s: %s' % (m, e))
        for m in ('ListSets', 'ListVaultOutputs'):
            try:
                grpc('cash.z.wallet.sdk.rpc.YellowbackStreamer/' + m, {} if m == 'ListVaultOutputs' else None, stream=True)
                check(True, 'YellowbackStreamer/%s' % m)
            except Exception as e:
                check(False, 'YellowbackStreamer/%s: %s' % (m, e))
        check(rpc('yed_getinfo')['rpcversion'] == 12 and rpc('yugen_getinfo')['rpcversion'] == 12,
              'node rpcversion 12 (yed_getinfo, yugen_getinfo), and the server started its service on it')
        log = open(lwd_log).read()
        bad = [l for l in log.splitlines() if 'overlong' in l or 'error parsing block' in l or 'level=fatal' in l]
        check(not bad, 'server log: no block parse error%s' % (': ' + bad[0][:200] if bad else ''))
    finally:
        if lwd:
            lwd.terminate()
            lwd.wait()
        try:
            rpc('stop')
        except Exception:
            node.terminate()
        node.wait(timeout=120)
        if not FAIL:
            shutil.rmtree(work, ignore_errors=True)
        else:
            print('kept', work)
    print('%d failed' % len(FAIL))
    return 1 if FAIL else 0


def run(cmd):
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=1800)
    if out.returncode != 0:
        raise RuntimeError('%s: %s %s' % (cmd[:2], out.stdout, out.stderr[-2000:]))
    return out.stdout.strip()


if __name__ == '__main__':
    sys.exit(main())
