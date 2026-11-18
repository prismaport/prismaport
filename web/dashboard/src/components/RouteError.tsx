import { useRouteError } from 'react-router-dom';
import { AuthLayout } from './AuthLayout';
import { ErrorState, PageHead } from './ui';

const reload = () => window.location.reload();

/** A dashboard page that failed to render, shown inside the shell so the navigation stays. */
export function PageError() {
  const error = useRouteError();
  return (
    <div className="stack">
      <PageHead title="This page could not be shown" />
      <ErrorState error={error} onRetry={reload} />
    </div>
  );
}

/** A sign-in page, or the shell itself, that failed to render. */
export function AppError() {
  const error = useRouteError();
  return (
    <AuthLayout title="This page could not be shown">
      <ErrorState error={error} onRetry={reload} />
    </AuthLayout>
  );
}
