import { Link, useNavigate } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";

export function Header() {
  const { session, logout } = useAuth();
  const navigate = useNavigate();

  return (
    <header className="header">
      <Link to="/" className="brand">
        USDT Convert
      </Link>
      <nav>
        {session ? (
          <>
            <span className="hint">{session.email}</span>
            <button
              className="link"
              onClick={async () => {
                await logout();
                navigate("/login");
              }}
            >
              Log out
            </button>
          </>
        ) : (
          <>
            <Link to="/login">Log in</Link>
            <Link to="/register">Sign up</Link>
          </>
        )}
      </nav>
    </header>
  );
}
