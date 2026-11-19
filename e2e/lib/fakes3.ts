import crypto from 'node:crypto';
import http from 'node:http';

// THE e2e FAKE S3, one for every spec whose server has an upload store
// (uploads, pages, ask-media, opencode-ask-media). Path-style, one bucket,
// what minio-go (internal/uploads) sends: HEAD bucket, ?lifecycle,
// ?location, PUT (aws-chunked when the payload is streamed), HEAD, GET and
// DELETE object. Multipart uploads are not played: every file is far under
// one part. A PUT is checked as S3 checks it: a streamed body's decoded
// length must match x-amz-decoded-content-length, any other's SHA-256 must
// match x-amz-content-sha256 (unless UNSIGNED-PAYLOAD).
//
//   const s3 = await startFakeS3(port, ACCESS, 'muster-e2e');
//   … MUSTER_S3_ENDPOINT: `http://127.0.0.1:${port}`, MUSTER_S3_BUCKET: 'muster-e2e' …
//   s3.objects (key → bytes), s3.log ("<METHOD> <path>[?query]", in order)
//   await s3.close();

export interface FakeS3 {
  objects: Map<string, Buffer>;
  log: string[];
  close(): Promise<void>;
}

const xmlErr = (res: http.ServerResponse, status: number, code: string) =>
  res.writeHead(status, { 'Content-Type': 'application/xml' }).end(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>${code}</Code><Message>${code}</Message></Error>`);

// unchunk decodes an aws-chunked body: `<hex size>[;chunk-signature=…]\r\n<data>\r\n` … `0…`.
function unchunk(b: Buffer): Buffer {
  const out: Buffer[] = [];
  let at = 0;
  for (;;) {
    const eol = b.indexOf('\r\n', at);
    if (eol < 0) break;
    const size = parseInt(b.subarray(at, eol).toString('latin1').split(';')[0], 16);
    if (!size) break;
    out.push(b.subarray(eol + 2, eol + 2 + size));
    at = eol + 2 + size + 2;
  }
  return Buffer.concat(out);
}

const objHeaders = (b: Buffer) => ({
  'Content-Length': String(b.length),
  ETag: '"' + crypto.createHash('md5').update(b).digest('hex') + '"',
  'Last-Modified': new Date().toUTCString(),
  'Content-Type': 'application/octet-stream',
});

export async function startFakeS3(port: number, access: string, bucket: string): Promise<FakeS3> {
  const objects = new Map<string, Buffer>();
  const log: string[] = [];
  const prefix = '/' + bucket;
  const srv = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => {
      let body = Buffer.concat(chunks);
      const [rawPath, query = ''] = (req.url ?? '').split('?');
      const p = decodeURIComponent(rawPath);
      log.push(`${req.method} ${p}${query ? '?' + query : ''}`);
      if (!(req.headers.authorization ?? '').startsWith(`AWS4-HMAC-SHA256 Credential=${access}/`)) return xmlErr(res, 403, 'AccessDenied');
      if (p !== prefix && !p.startsWith(prefix + '/')) return xmlErr(res, 404, 'NoSuchBucket');
      const key = p.slice(prefix.length).replace(/^\//, '');
      if (key === '') {
        if (req.method === 'HEAD') return void res.writeHead(200).end();
        if (query.startsWith('lifecycle')) return xmlErr(res, 404, 'NoSuchLifecycleConfiguration');
        if (query.startsWith('location')) return void res.writeHead(200, { 'Content-Type': 'application/xml' }).end('<LocationConstraint>us-east-1</LocationConstraint>');
        return xmlErr(res, 400, 'NotImplemented');
      }
      if (req.method === 'PUT') {
        const declared = String(req.headers['x-amz-content-sha256'] ?? '');
        if (declared.startsWith('STREAMING')) {
          body = unchunk(body);
          if (String(body.length) !== String(req.headers['x-amz-decoded-content-length'])) return xmlErr(res, 400, 'IncompleteBody');
        } else if (declared !== 'UNSIGNED-PAYLOAD' && declared !== crypto.createHash('sha256').update(body).digest('hex')) {
          return xmlErr(res, 400, 'XAmzContentSHA256Mismatch');
        }
        objects.set(key, body);
        return void res.writeHead(200, { ETag: objHeaders(body).ETag }).end();
      }
      const b = objects.get(key);
      if (req.method === 'HEAD') return b ? void res.writeHead(200, objHeaders(b)).end() : void res.writeHead(404).end();
      if (req.method === 'GET') return b ? void res.writeHead(200, objHeaders(b)).end(b) : xmlErr(res, 404, 'NoSuchKey');
      if (req.method === 'DELETE') {
        objects.delete(key);
        return void res.writeHead(204).end();
      }
      res.writeHead(405).end();
    });
  });
  await new Promise<void>((r) => srv.listen(port, '127.0.0.1', () => r()));
  return { objects, log, close: () => new Promise((r) => srv.close(() => r())) };
}
