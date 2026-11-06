"""Diagnostic only: use the existing community key through curl, never log URLs/keys.

Every attempt gets a fresh signed upload URL. Stop after the first accepted wall
post; leave all created objects for the owner's manual inspection.
"""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import urllib.parse


class ProbeError(Exception):
    """Messages must contain only locally constructed safe diagnostics."""


def quote(value):
    return '"' + str(value).replace('\\', '\\\\').replace('"', '\\"').replace('\n', '\\n').replace('\r', '\\r') + '"'


def request(endpoint, fields=None, upload=None, http1=False):
    config = ['url = ' + quote(endpoint), 'request = "POST"']
    for key, value in (fields or {}).items():
        config.append('data-urlencode = ' + quote(key + '=' + str(value)))
    if upload:
        path, field, filename, mime = upload
        form = field + '=@' + str(path) + ';filename=' + filename
        if mime:
            form += ';type=' + mime
        config.append('form = ' + quote(form))
    command = ['curl', '--silent', '--show-error', '--max-time', '20',
               '--proto', '=https', '--config', '-', '--write-out', '\n%{http_code}']
    if http1:
        command.append('--http1.1')
    # Credentials and signed URLs are in stdin, not argv. Never print stderr/body.
    result = subprocess.run(command, input=('\n'.join(config) + '\n').encode(), capture_output=True)
    if result.returncode:
        raise ProbeError('curl_exit=' + str(result.returncode))
    body, _, status = result.stdout.rpartition(b'\n')
    if status != b'200':
        raise ProbeError('HTTP=' + (status.decode() if status.isdigit() else 'unknown'))
    try:
        payload = json.loads(body)
    except (ValueError, UnicodeError):
        raise ProbeError('HTTP=200 invalid_JSON') from None
    if not isinstance(payload, dict):
        raise ProbeError('HTTP=200 unexpected_JSON_type')
    return payload


def vk(method, token, version, **fields):
    payload = request('https://api.vk.com/method/' + method,
                      dict(access_token=token, v=version, **fields))
    if 'error' in payload:
        code = payload['error'].get('error_code', 0)
        raise ProbeError(method + ' API_code=' + str(code if isinstance(code, int) else 0))
    if 'response' not in payload:
        raise ProbeError(method + ' missing_response')
    return payload['response']


def trusted(endpoint):
    parsed = urllib.parse.urlparse(endpoint)
    hosts = ('vk.com', 'vk.ru', 'vkuserphoto.ru', 'vkuserphoto.com', 'userapi.com')
    if (parsed.scheme != 'https' or parsed.username or parsed.password or parsed.fragment
            or not any(parsed.hostname == h or (parsed.hostname or '').endswith('.' + h) for h in hosts)):
        raise ProbeError('untrusted_upload_endpoint')


def cases():
    # Test one variable at a time against the small image with its correct MIME.
    for width in (320, 1280):
        for ext, mime in (('jpg', 'image/jpeg'), ('png', 'image/png')):
            yield width, ext, 'photo', 'photo.' + ext, mime, '5.199', False
    for ext in ('jpg', 'png'):
        for mime in ('application/octet-stream', None):
            yield 320, ext, 'photo', 'photo.' + ext, mime, '5.199', False
        for field in ('file', 'file0', 'file1'):
            yield 320, ext, field, 'photo.' + ext, 'image/jpeg' if ext == 'jpg' else 'image/png', '5.199', False
        yield 320, ext, 'photo', 'photo.' + ext, 'image/jpeg' if ext == 'jpg' else 'image/png', '5.131', False
        yield 320, ext, 'photo', 'photo.' + ext, 'image/jpeg' if ext == 'jpg' else 'image/png', '5.199', True
    yield 320, 'jpg', 'photo', 'photo.jpeg', 'image/jpeg', '5.199', False


def main():
    token = os.environ['VK_TEST_COMMUNITY_TOKEN']
    group = int(os.environ['VK_TEST_GROUP_ID'])
    fixtures = Path(sys.argv[1])
    own = vk('groups.getById', token, '5.199')
    own = own.get('groups', []) if isinstance(own, dict) else own
    if group <= 0 or len(own) != 1 or own[0]['id'] != group:
        raise ProbeError('community_identity_mismatch')
    # Check the intended wall route once; don't repeat a known auth denial per file.
    try:
        vk('photos.getWallUploadServer', token, '5.199', group_id=group)
    except ProbeError as error:
        print('::notice title=VK wall upload route::' + str(error), flush=True)

    attempts = []
    for number, (width, ext, field, filename, mime, version, http1) in enumerate(cases(), 1):
        label = (f'{number}: {ext} {width}x{width * 3 // 4}, field={field}, '
                 f'filename={filename}, mime={mime or "curl-auto"}, v={version}, HTTP1={http1}')
        stage = 'getMessagesUploadServer'
        try:
            endpoint = vk('photos.getMessagesUploadServer', token, version)['upload_url']
            trusted(endpoint)
            stage = 'upload'
            uploaded = request(endpoint, upload=(fixtures / f'photo-{width}.{ext}', field, filename, mime), http1=http1)
            photo = uploaded.get('photo')
            if not isinstance(photo, str) or not photo or photo == '[]':
                state = 'empty_array' if photo == '[]' or photo == [] else 'missing_or_invalid'
                server = uploaded.get('server')
                raise ProbeError(f'HTTP=200 photo={state} photo_type={type(photo).__name__} photo_nonempty={bool(photo)} server_valid={isinstance(server, int) and server > 0} hash_present={bool(uploaded.get("hash"))}')
            stage = 'saveMessagesPhoto'
            saved = vk('photos.saveMessagesPhoto', token, version, photo=photo,
                       server=uploaded.get('server', 0), hash=uploaded.get('hash', ''))
            if len(saved) != 1 or saved[0].get('id', 0) <= 0 or not saved[0].get('owner_id'):
                raise ProbeError('photo_save_missing_ID')
            item = saved[0]
            attachment = f'photo{item["owner_id"]}_{item["id"]}'
            if item.get('access_key'):
                attachment += '_' + item['access_key']
            marker = 'autoposter-ci-curl-formats-' + os.environ['GITHUB_RUN_ID']
            # Same guid for every case: an uncertain response must not cause duplicates.
            guid = hashlib.sha256((marker + ':' + os.environ.get('GITHUB_RUN_ATTEMPT', '1')).encode()).hexdigest()[:32]
            stage = 'wall.post'
            posted = vk('wall.post', token, version, owner_id=-group, from_group=1,
                        attachments=attachment, message=marker, guid=guid)
            post_id = posted.get('post_id', 0)
            if not isinstance(post_id, int) or post_id <= 0:
                raise ProbeError('wall_post_missing_ID')
            print(f'::notice title=VK photo format accepted::{label}; created https://vk.ru/wall-{group}_{post_id}; left for manual inspection', flush=True)
            if attempts:
                print('::notice title=VK photo preceding attempts::' + '%0A'.join(attempts), flush=True)
            return
        except ProbeError as error:
            diagnostic = f'{label}; stage={stage}; {error}'
            attempts.append(diagnostic)
            # GitHub only retains ten notice annotations per step. Aggregate
            # failures so the successful variant and its post URL are retained.
            print(diagnostic, flush=True)
            if stage == 'wall.post':
                # An uncertain wall.post can already have created a post; don't submit
                # another attachment or switch API versions after that point.
                print('::notice title=VK photo preceding attempts::' + '%0A'.join(attempts), flush=True)
                raise ProbeError('wall_post_failed_or_uncertain; stopped_after_first_publication_attempt') from None
    print('::notice title=VK photo format attempts::' + '%0A'.join(attempts), flush=True)
    raise ProbeError('no_format_accepted; no_wall_post_request_sent')


if __name__ == '__main__':
    try:
        main()
    except ProbeError as error:
        print('::error title=VK photo format probe::' + str(error), flush=True)
        sys.exit(1)
    except Exception as error:
        print('::error title=VK photo format probe::local_error=' + type(error).__name__, flush=True)
        sys.exit(1)
