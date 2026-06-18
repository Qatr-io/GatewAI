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
          label: 'Architecture',
          items: [
            { label: 'Overview',         slug: 'architecture/overview' },
            { label: 'Request flows',    slug: 'architecture/request-flows' },
            { label: 'Service registry', slug: 'architecture/service-registry' },
            { label: 'Priority routing', slug: 'architecture/priority' },
            { label: 'LLM proxy',        slug: 'architecture/llm-proxy' },
            { label: 'Rate limiting',    slug: 'architecture/rate-limiting' },
            { label: 'PII guardrails',   slug: 'architecture/guardrails' },
            { label: 'Audit log',        slug: 'architecture/audit-log' },
          ],
        },
        {
          label: 'Deployment',
          items: [
            { label: 'Helm chart',              slug: 'deployment/helm' },
            { label: 'Configuration reference', slug: 'deployment/configuration' },
            { label: 'Relay queue (Redis)',      slug: 'deployment/queue' },
          ],
        },
        {
          label: 'Contributing',
          items: [
            { label: 'Gitflow',   slug: 'contributing/gitflow' },
            { label: 'Releasing', slug: 'contributing/releasing' },
          ],
        },
        {
          label: 'Runbooks',
          items: [{ autogenerate: { directory: 'runbooks' } }],
        },
        { label: 'Changelog', slug: 'changelog' },
      ],
    }),
  ],
});
