import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

export default defineConfig({
  site: 'https://qatr-io.github.io',
  base: '/GatewAI',
  integrations: [
    starlight({
      title: 'GatewAI',
      components: {
        SiteTitle: './src/components/SiteTitle.astro',
        Hero: './src/components/Hero.astro',
      },
      customCss: ['./src/styles/landing.css'],
      social: [
        { icon: 'github', label: 'GitHub', href: 'https://github.com/Qatr-io/GatewAI' },
      ],
      sidebar: [
        {
          label: 'Get started',
          items: [
            { label: 'Overview',       slug: 'get-started/overview' },
            { label: 'Request flows',  slug: 'get-started/request-flows' },
          ],
        },
        {
          label: 'Set up',
          items: [
            { label: 'Helm chart',        slug: 'set-up/helm' },
            { label: 'Relay queue',       slug: 'set-up/queue' },
          ],
        },
        {
          label: 'Configure',
          items: [
            { label: 'Configuration reference', slug: 'configure/configuration' },
            { label: 'Service registry',        slug: 'configure/service-registry' },
            { label: 'Priority routing',        slug: 'configure/priority-routing' },
            { label: 'LLM proxy',               slug: 'configure/llm-proxy' },
            { label: 'Rate limiting',           slug: 'configure/rate-limiting' },
            { label: 'PII guardrails',          slug: 'configure/guardrails' },
            { label: 'Audit log',               slug: 'configure/audit-log' },
          ],
        },
        {
          label: 'Manage',
          items: [
            { label: 'Runbooks', slug: 'manage/runbooks' },
          ],
        },
        {
          label: 'Reference',
          items: [
            { label: 'Changelog', slug: 'reference/changelog' },
          ],
        },
        {
          label: 'Community',
          items: [
            { label: 'Gitflow',   slug: 'community/gitflow' },
            { label: 'Releasing', slug: 'community/releasing' },
          ],
        },
      ],
    }),
  ],
});
