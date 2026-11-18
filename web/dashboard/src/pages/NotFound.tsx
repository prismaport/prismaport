import { Link as RouterLink } from 'react-router-dom';
import { PageHead } from '../components/ui';

export function NotFoundPage() {
  return (
    <div className="not-found">
      <PageHead title="Page not found" lead="This page does not exist in the dashboard. Check the address or go back to your payment links." />
      <RouterLink to="/links" className="btn">
        Payment links
      </RouterLink>
    </div>
  );
}
