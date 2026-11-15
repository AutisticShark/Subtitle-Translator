export type Theme = 'system' | 'light' | 'dark';
export type View = 'dashboard' | 'translate' | 'jobs' | 'security' | 'admin';
export interface User {
  id: string;
  username: string;
  role: 'admin' | 'user';
  theme: Theme;
  active: boolean;
  locked: boolean;
  created_at: string;
  job_count?: number;
}
export interface CaptchaConfig {
  provider: 'none' | 'turnstile' | 'recaptcha' | 'hcaptcha';
  site_key: string;
  protected_actions: string[];
}
export interface SetupStatus {
  configured: boolean;
  registration_enabled: boolean;
  captcha: CaptchaConfig;
}
export interface Settings {
  providers: Record<string, string>;
  configured: Record<string, boolean>;
  captcha_configured: Record<string, boolean>;
  [key: string]: unknown;
}
export interface Catalog {
  locale: string;
  messages: Record<string, string>;
  languages: Record<string, string>;
}
export interface Challenge {
  challenge_token: string;
  method: 'totp' | 'email';
  expires_in: number;
  email_sent?: boolean;
  warning?: string;
  secret?: string;
  qr_code?: string;
  provisioning_uri?: string;
}
export interface AuthResponse extends Partial<Challenge> {
  user?: User;
  mfa_required?: boolean;
  recovery_codes?: string[];
}
export interface MfaStatus {
  method: '' | 'totp' | 'email';
  email: string;
  recovery_remaining: number;
  email_available: boolean;
  enrollment_available: boolean;
}
export type JobStatus = 'queued' | 'processing' | 'canceling' | 'completed' | 'failed' | 'canceled';
export interface Job {
  id: string;
  filename: string;
  status: JobStatus;
  progress: number;
  stage: string;
  options: {
    provider: string;
    model: string | null;
    source_language: string;
    target_languages: string[];
    encoding: string;
  };
  outputs: { name: string; language: string }[];
  error: string | null;
  warning: string | null;
  created_at: string;
  updated_at: string;
  owner?: string | null;
}
export interface JobPage {
  jobs: Job[];
  total: number;
  counts: Partial<Record<JobStatus, number>>;
  offset: number;
  limit: number;
  has_more: boolean;
}
