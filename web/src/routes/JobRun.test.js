import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { token, serverURL } from '../lib/api.js';
import JobRun from './JobRun.svelte';

function jsonResponse(body) {
  return Promise.resolve({
    ok: true,
    status: 200,
    json: async () => body,
    text: async () => JSON.stringify(body),
  });
}

beforeEach(() => {
  token.set('');
  serverURL.set('http://localhost:8080');
});

// choices-carrying inputs (Layer 3 / Web UI): a job input that carries a
// non-empty `choices` array must render as a <select> dropdown instead of
// the usual text/number input, with a blank placeholder option so an
// optional choices param can represent "unset".
describe('JobRun — choices dropdown for job inputs', () => {
  it('renders a <select> (not a text input) for a string input with choices', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/api/v1/jobs/')) {
        return jsonResponse({
          name: 'deploy',
          inputs: [
            { name: 'env', type: 'string', required: true, choices: ['staging', 'prod'] },
          ],
        });
      }
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobRun, { props: { params: { name: 'deploy' } } });

    await vi.waitFor(() => {
      expect(container.querySelector('select')).toBeTruthy();
    });

    // No plain text input for this choices-carrying param.
    expect(container.querySelector('input[type="text"]')).toBeFalsy();

    const select = container.querySelector('select');
    const options = [...select.querySelectorAll('option')];
    // Blank placeholder + one option per choice.
    expect(options.length).toBe(3);
    expect(options[0].value).toBe('');
    expect(options.map((o) => o.value)).toEqual(expect.arrayContaining(['staging', 'prod']));
    expect(options.map((o) => o.textContent)).toEqual(
      expect.arrayContaining(['staging', 'prod']),
    );
  });

  it('sends the selected choice value in the POST body to /api/v1/runs', async () => {
    let capturedBody = null;
    const fetchMock = vi.fn((url, options) => {
      const u = String(url);
      if (u.includes('/api/v1/jobs/')) {
        return jsonResponse({
          name: 'deploy',
          inputs: [
            { name: 'env', type: 'string', required: true, choices: ['staging', 'prod'] },
          ],
        });
      }
      if (u.includes('/api/v1/runs')) {
        capturedBody = JSON.parse(options.body);
        return jsonResponse({ id: 'run-1' });
      }
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobRun, { props: { params: { name: 'deploy' } } });

    await vi.waitFor(() => {
      expect(container.querySelector('select')).toBeTruthy();
    });

    const select = container.querySelector('select');
    await fireEvent.change(select, { target: { value: 'prod' } });

    // AuthSetup also renders a "button.btn" ("Save"), so find the Run button
    // by its label rather than assuming it's the only/first .btn on the page.
    const button = [...container.querySelectorAll('button.btn')].find((b) =>
      b.textContent.includes('Run'),
    );
    await fireEvent.click(button);

    await vi.waitFor(() => {
      expect(capturedBody).toBeTruthy();
    });
    expect(capturedBody).toEqual({ jobName: 'deploy', params: { env: 'prod' } });
  });

  it('pre-selects the default option when the input carries both default and choices', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/api/v1/jobs/')) {
        return jsonResponse({
          name: 'deploy',
          inputs: [
            {
              name: 'env',
              type: 'string',
              required: false,
              default: 'staging',
              choices: ['staging', 'prod'],
            },
          ],
        });
      }
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobRun, { props: { params: { name: 'deploy' } } });

    await vi.waitFor(() => {
      expect(container.querySelector('select')).toBeTruthy();
    });

    const select = container.querySelector('select');
    expect(select.value).toBe('staging');
  });

  it('still renders a plain text/number/checkbox input for params without choices (regression)', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/api/v1/jobs/')) {
        return jsonResponse({
          name: 'deploy',
          inputs: [
            { name: 'name', type: 'string', required: false },
            { name: 'count', type: 'int', required: false, default: 3 },
            { name: 'force', type: 'bool', required: false, default: false },
          ],
        });
      }
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobRun, { props: { params: { name: 'deploy' } } });

    await vi.waitFor(() => {
      expect(container.querySelector('input[type="text"]')).toBeTruthy();
    });
    expect(container.querySelector('input[type="number"]')).toBeTruthy();
    expect(container.querySelector('input[type="checkbox"]')).toBeTruthy();
    expect(container.querySelector('select')).toBeFalsy();
  });

  it('leaves the Run button enabled when an optional choices input is left on the blank placeholder', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/api/v1/jobs/')) {
        return jsonResponse({
          name: 'deploy',
          inputs: [
            { name: 'env', type: 'string', required: false, choices: ['staging', 'prod'] },
          ],
        });
      }
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobRun, { props: { params: { name: 'deploy' } } });

    await vi.waitFor(() => {
      expect(container.querySelector('select')).toBeTruthy();
    });

    const button = [...container.querySelectorAll('button.btn')].find((b) =>
      b.textContent.includes('Run'),
    );
    expect(button.disabled).toBe(false);
  });
});
