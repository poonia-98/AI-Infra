import { render, screen } from '@testing-library/react';
import StatusBadge from '../StatusBadge';

describe('StatusBadge', () => {
  it('renders running status correctly', () => {
    render(<StatusBadge status="running" />);
    expect(screen.getByText('running')).toBeInTheDocument();
  });

  it('renders stopped status correctly', () => {
    render(<StatusBadge status="stopped" />);
    expect(screen.getByText('stopped')).toBeInTheDocument();
  });
});