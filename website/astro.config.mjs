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
          items: [{ label: 'Overview', slug: 'get-started' }],
        },
        {
          label: 'Set up',
          items: [{ label: 'Overview', slug: 'set-up' }],
        },
        {
          label: 'Configure',
          items: [{ label: 'Overview', slug: 'configure' }],
        },
        {
          label: 'Manage',
          items: [{ label: 'Overview', slug: 'manage' }],
        },
        {
          label: 'Reference',
          items: [{ label: 'Overview', slug: 'reference' }],
        },
        {
          label: 'Community',
          items: [{ label: 'Overview', slug: 'community' }],
        },
      ],
    }),
  ],
});
